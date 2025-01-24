/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package instancetype

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/aws/karpenter-provider-aws/pkg/providers/amifamily"

	"github.com/mitchellh/hashstructure/v2"
	"github.com/patrickmn/go-cache"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/util/sets"

	v1 "github.com/aws/karpenter-provider-aws/pkg/apis/v1"
	sdk "github.com/aws/karpenter-provider-aws/pkg/aws"

	"github.com/aws/karpenter-provider-aws/pkg/providers/subnet"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/utils/pretty"
)

type Provider interface {
	List(context.Context, *v1.EC2NodeClass) ([]*cloudprovider.InstanceType, error)
}

type DefaultProvider struct {
	ec2api                sdk.EC2API
	subnetProvider        subnet.Provider
	instanceTypesResolver Resolver

	// Values stored *before* considering insufficient capacity errors from the unavailableOfferings cache.
	// Fully initialized Instance Types are also cached based on the set of all instance types, zones, unavailableOfferings cache,
	// EC2NodeClass, and kubelet configuration from the NodePool

	muInstanceTypesInfo sync.RWMutex
	// TODO @engedaam: Look into only storing the needed EC2InstanceTypeInfo
	instanceTypesInfo []ec2types.InstanceTypeInfo

	muInstanceTypesOfferings sync.RWMutex
	instanceTypesOfferings   map[string]sets.Set[string]

	instanceTypesCache      *cache.Cache
	discoveredCapacityCache *cache.Cache
	cm                      *pretty.ChangeMonitor
	// instanceTypesSeqNum is a monotonically increasing change counter used to avoid the expensive hashing operation on instance types
	instanceTypesSeqNum uint64
	// instanceTypesOfferingsSeqNum is a monotonically increasing change counter used to avoid the expensive hashing operation on instance types
	instanceTypesOfferingsSeqNum uint64

	// mutex required here to allow synchronization with other operations (not possible with atomics only)
	muLastUnavailableOfferingsSeqNum sync.Mutex
	// lastUnavailableOfferingsSeqNum is the most recently seen seq num of the unavailable offerings cache, used to track changes
	lastUnavailableOfferingsSeqNum uint64
}

func NewDefaultProvider(instanceTypesCache *cache.Cache, discoveredCapacityCache *cache.Cache, ec2api sdk.EC2API, subnetProvider subnet.Provider, instanceTypesResolver Resolver) *DefaultProvider {
	return &DefaultProvider{
		ec2api:                  ec2api,
		subnetProvider:          subnetProvider,
		instanceTypesInfo:       []ec2types.InstanceTypeInfo{},
		instanceTypesOfferings:  map[string]sets.Set[string]{},
		instanceTypesResolver:   instanceTypesResolver,
		instanceTypesCache:      instanceTypesCache,
		discoveredCapacityCache: discoveredCapacityCache,
		cm:                      pretty.NewChangeMonitor(),
		instanceTypesSeqNum:     0,
	}
}

//nolint:gocyclo
func (p *DefaultProvider) List(ctx context.Context, nodeClass *v1.EC2NodeClass) ([]*cloudprovider.InstanceType, error) {
	p.muInstanceTypesInfo.RLock()
	p.muInstanceTypesOfferings.RLock()
	defer p.muInstanceTypesInfo.RUnlock()
	defer p.muInstanceTypesOfferings.RUnlock()

	if len(p.instanceTypesInfo) == 0 {
		return nil, fmt.Errorf("no instance types found")
	}
	if len(p.instanceTypesOfferings) == 0 {
		return nil, fmt.Errorf("no instance types offerings found")
	}
	if len(nodeClass.Status.Subnets) == 0 {
		return nil, fmt.Errorf("no subnets found")
	}

	subnetZones := sets.New(lo.Map(nodeClass.Status.Subnets, func(s v1.Subnet, _ int) string {
		return lo.FromPtr(&s.Zone)
	})...)

	// Compute fully initialized instance types hash key
	subnetZonesHash, _ := hashstructure.Hash(subnetZones, hashstructure.FormatV2, &hashstructure.HashOptions{SlicesAsSets: true})

	// Compute hash key against node class AMIs (used to force cache rebuild when AMIs change)
	amiHash, _ := hashstructure.Hash(nodeClass.Status.AMIs, hashstructure.FormatV2, &hashstructure.HashOptions{SlicesAsSets: true})

	// Store first observed value of seqNum before instance type resolution to track modification
	unavailableOfferingsSeqNum := p.instanceTypesResolver.GetUnavailableOfferingsSeqNum()

	key := fmt.Sprintf("%d-%d-%016x-%016x-%016x",
		p.instanceTypesSeqNum,
		p.instanceTypesOfferingsSeqNum,
		amiHash,
		subnetZonesHash,
		p.instanceTypesResolver.CacheKey(nodeClass),
	)

	if item, ok := p.instanceTypesCache.Get(key); ok {
		// Ensure what's returned from this function is a shallow-copy of the slice (not a deep-copy of the data itself)
		// so that modifications to the ordering of the data don't affect the original
		return append([]*cloudprovider.InstanceType{}, item.([]*cloudprovider.InstanceType)...), nil
	}

	// Get all zones across all offerings
	// We don't use this in the cache key since this is produced from our instanceTypesOfferings which we do cache
	allZones := sets.New[string]()
	for _, offeringZones := range p.instanceTypesOfferings {
		for zone := range offeringZones {
			allZones.Insert(zone)
		}
	}
	if p.cm.HasChanged("zones", allZones) {
		log.FromContext(ctx).WithValues("zones", allZones.UnsortedList()).V(1).Info("discovered zones")
	}
	subnetZoneToID := lo.SliceToMap(nodeClass.Status.Subnets, func(s v1.Subnet) (string, string) {
		return s.Zone, s.ZoneID
	})
	result := lo.Map(p.instanceTypesInfo, func(i ec2types.InstanceTypeInfo, _ int) *cloudprovider.InstanceType {
		InstanceTypeVCPU.Set(float64(lo.FromPtr(i.VCpuInfo.DefaultVCpus)), map[string]string{
			instanceTypeLabel: string(i.InstanceType),
		})
		InstanceTypeMemory.Set(float64(lo.FromPtr(i.MemoryInfo.SizeInMiB)*1024*1024), map[string]string{
			instanceTypeLabel: string(i.InstanceType),
		})

		zoneData := lo.Map(allZones.UnsortedList(), func(zoneName string, _ int) ZoneData {
			if !p.instanceTypesOfferings[string(i.InstanceType)].Has(zoneName) || !subnetZones.Has(zoneName) {
				return ZoneData{
					Name:      zoneName,
					Available: false,
				}
			}
			return ZoneData{
				Name:      zoneName,
				ID:        subnetZoneToID[zoneName],
				Available: true,
			}
		})

		it := p.instanceTypesResolver.Resolve(ctx, i, zoneData, nodeClass)
		if cached, ok := p.discoveredCapacityCache.Get(fmt.Sprintf("%s-%016x", it.Name, amiHash)); ok {
			it.Capacity[corev1.ResourceMemory] = cached.(resource.Quantity)
		}
		for _, of := range it.Offerings {
			InstanceTypeOfferingAvailable.Set(float64(lo.Ternary(of.Available, 1, 0)), map[string]string{
				instanceTypeLabel: it.Name,
				capacityTypeLabel: of.Requirements.Get(karpv1.CapacityTypeLabelKey).Any(),
				zoneLabel:         of.Requirements.Get(corev1.LabelTopologyZone).Any(),
			})
			InstanceTypeOfferingPriceEstimate.Set(of.Price, map[string]string{
				instanceTypeLabel: it.Name,
				capacityTypeLabel: of.Requirements.Get(karpv1.CapacityTypeLabelKey).Any(),
				zoneLabel:         of.Requirements.Get(corev1.LabelTopologyZone).Any(),
			})
		}
		return it
	})

	p.muLastUnavailableOfferingsSeqNum.Lock()

	// Flush the cache if the current unavailable offerings seq num has changed since the last flush
	if seqNum := p.instanceTypesResolver.GetUnavailableOfferingsSeqNum(); p.lastUnavailableOfferingsSeqNum < seqNum {
		p.instanceTypesCache.Flush()
		p.lastUnavailableOfferingsSeqNum = seqNum
		log.FromContext(ctx).Info("Instance types cache flushed")
	}

	// Only cache the result if the seq num has not changed since the key was formed
	if p.lastUnavailableOfferingsSeqNum == unavailableOfferingsSeqNum {
		p.instanceTypesCache.SetDefault(key, result)
		log.FromContext(ctx).Info(fmt.Sprintf("Added key \"%s\" to instance types cache", key))
	}

	p.muLastUnavailableOfferingsSeqNum.Unlock()

	return result, nil
}

func (p *DefaultProvider) UpdateInstanceTypes(ctx context.Context) error {
	// DO NOT REMOVE THIS LOCK ----------------------------------------------------------------------------
	// We lock here so that multiple callers to getInstanceTypeOfferings do not result in cache misses and multiple
	// calls to EC2 when we could have just made one call.
	// TODO @joinnis: This can be made more efficient by holding a Read lock and only obtaining the Write if not in cache
	p.muInstanceTypesInfo.Lock()
	defer p.muInstanceTypesInfo.Unlock()

	var instanceTypes []ec2types.InstanceTypeInfo

	paginator := ec2.NewDescribeInstanceTypesPaginator(p.ec2api, &ec2.DescribeInstanceTypesInput{
		Filters: []ec2types.Filter{
			{
				Name:   aws.String("supported-virtualization-type"),
				Values: []string{"hvm"},
			},
			{
				Name:   aws.String("processor-info.supported-architecture"),
				Values: []string{"x86_64", "arm64"},
			},
		},
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("describing instance types, %w", err)
		}

		instanceTypes = append(instanceTypes, page.InstanceTypes...)
	}

	if p.cm.HasChanged("instance-types", instanceTypes) {
		// Only update instanceTypesSeqNum with the instance types have been changed
		// This is to not create new keys with duplicate instance types option
		atomic.AddUint64(&p.instanceTypesSeqNum, 1)
		log.FromContext(ctx).WithValues(
			"count", len(instanceTypes)).V(1).Info("discovered instance types")
	}
	p.instanceTypesInfo = instanceTypes
	return nil
}

func (p *DefaultProvider) UpdateInstanceTypeOfferings(ctx context.Context) error {
	// DO NOT REMOVE THIS LOCK ----------------------------------------------------------------------------
	// We lock here so that multiple callers to GetInstanceTypes do not result in cache misses and multiple
	// calls to EC2 when we could have just made one call. This lock is here because multiple callers to EC2 result
	// in A LOT of extra memory generated from the response for simultaneous callers.
	// TODO @joinnis: This can be made more efficient by holding a Read lock and only obtaining the Write if not in cache
	p.muInstanceTypesOfferings.Lock()
	defer p.muInstanceTypesOfferings.Unlock()

	// Get offerings from EC2
	instanceTypeOfferings := map[string]sets.Set[string]{}

	paginator := ec2.NewDescribeInstanceTypeOfferingsPaginator(p.ec2api, &ec2.DescribeInstanceTypeOfferingsInput{
		LocationType: ec2types.LocationTypeAvailabilityZone,
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("describing instance type zone offerings, %w", err)
		}

		for _, offering := range page.InstanceTypeOfferings {
			if _, ok := instanceTypeOfferings[string(offering.InstanceType)]; !ok {
				instanceTypeOfferings[string(offering.InstanceType)] = sets.New[string]()
			}
			instanceTypeOfferings[string(offering.InstanceType)].Insert(lo.FromPtr(offering.Location))
		}
	}

	if p.cm.HasChanged("instance-type-offering", instanceTypeOfferings) {
		// Only update instanceTypesSeqNun with the instance type offerings  have been changed
		// This is to not create new keys with duplicate instance type offerings option
		atomic.AddUint64(&p.instanceTypesOfferingsSeqNum, 1)
		log.FromContext(ctx).WithValues("instance-type-count", len(instanceTypeOfferings)).V(1).Info("discovered offerings for instance types")
	}
	p.instanceTypesOfferings = instanceTypeOfferings
	return nil
}

func (p *DefaultProvider) UpdateInstanceTypeCapacityFromNode(ctx context.Context, node *corev1.Node, nodeClaim *karpv1.NodeClaim, nodeClass *v1.EC2NodeClass) error {

	// Get mappings for most recent AMIs
	instanceTypeName := node.Labels[corev1.LabelInstanceTypeStable]
	amiMap := amifamily.MapToInstanceTypes([]*cloudprovider.InstanceType{{
		Name:         instanceTypeName,
		Requirements: scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaim.Spec.Requirements...),
	}}, nodeClass.Status.AMIs)
	// Ensure NodeClaim AMI is current
	if !lo.ContainsBy(amiMap[nodeClaim.Status.ImageID], func(i *cloudprovider.InstanceType) bool {
		return i.Name == instanceTypeName
	}) {
		return nil
	}

	amiHash, _ := hashstructure.Hash(nodeClass.Status.AMIs, hashstructure.FormatV2, &hashstructure.HashOptions{SlicesAsSets: true})
	key := fmt.Sprintf("%s-%016x", instanceTypeName, amiHash)

	// Update cache if non-existent or actual capacity is less than or equal to cached value
	actualCapacity := node.Status.Capacity.Memory()
	if cachedCapacity, ok := p.discoveredCapacityCache.Get(key); !ok || actualCapacity.Cmp(cachedCapacity.(resource.Quantity)) < 1 {
		log.FromContext(ctx).WithValues("memory-capacity", actualCapacity, "instance-type", instanceTypeName).V(1).Info("updating discovered capacity cache")
		p.discoveredCapacityCache.SetDefault(key, *actualCapacity)
	}
	return nil
}

func (p *DefaultProvider) Reset() {
	p.instanceTypesInfo = []ec2types.InstanceTypeInfo{}
	p.instanceTypesOfferings = map[string]sets.Set[string]{}
	p.instanceTypesCache.Flush()
	p.discoveredCapacityCache.Flush()
}
