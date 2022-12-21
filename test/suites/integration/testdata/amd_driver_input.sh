MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="BOUNDARY"

--BOUNDARY
Content-Type: text/x-shellscript; charset="us-ascii"

#!/bin/bash
cd
sudo amazon-linux-extras install epel -y
sudo yum update -y
cat << EOF > /tmp/amd-install.sh
#!/bin/bash
export echo PATH=/usr/local/bin:$PATH
aws s3 cp --recursive s3://ec2-amd-linux-drivers/latest/ . --no-sign-request
tar -xf amdgpu-pro-*rhel*.tar.xz
cd amdgpu-pro-20.20-1184451-rhel-7.8
./amdgpu-pro-install -y --opencl=pal,legacy
systemctl disable amd-install.service
reboot
EOF
sudo chmod +x /tmp/amd-install.sh
cat << EOF > /etc/systemd/system/amd-install.service
[Unit]
Description=install amd drivers

[Service]
ExecStart=/bin/bash /tmp/amd-install.sh

[Install]
WantedBy=multi-user.target
EOF
sudo systemctl enable amd-install.service
(sleep 60; sudo reboot) &
--BOUNDARY-- 