#!/bin/bash
# Generate a Dockerfile for the given base image.
# Usage: source this or call generate_dockerfile <base_image>

generate_dockerfile() {
	local base_image="$1"
	local dockerfile="$2"

	# Detect package manager from image name
	local install_cmd
	case "$base_image" in
	*ubuntu* | *debian*)
		install_cmd="sed -i 's|http://ports.ubuntu.com/ubuntu-ports/|http://mirrors.mit.edu/ubuntu-ports/|g' /etc/apt/sources.list.d/*.sources 2>/dev/null; sed -i 's|http://ports.ubuntu.com|http://mirrors.mit.edu/ubuntu-ports|g' /etc/apt/sources.list 2>/dev/null; apt-get update && apt-get install -y --no-install-recommends systemd xfsprogs openssh-server && apt-get clean && rm -rf /var/lib/apt/lists/*"
		;;
	*rocky* | *centos* | *alma* | *fedora* | *rhel* | *amazonlinux*)
		install_cmd="dnf install -y systemd xfsprogs openssh-server && dnf clean all"
		;;
	*alpine*)
		echo "Error: Alpine does not support systemd. Use Ubuntu, Debian, Rocky, or Fedora."
		exit 1
		;;
	*)
		install_cmd="apt-get update && apt-get install -y systemd xfsprogs openssh-server && apt-get clean && rm -rf /var/lib/apt/lists/*"
		;;
	esac

	cat >"$dockerfile" <<EOF
FROM ${base_image}

RUN ${install_cmd}

RUN mkdir -p /var/run/sshd /etc/systemd/system/multi-user.target.wants \
 && if [ -f /lib/systemd/system/ssh.service ]; then ln -sf /lib/systemd/system/ssh.service /etc/systemd/system/multi-user.target.wants/ssh.service; fi \
 && if [ -f /usr/lib/systemd/system/sshd.service ]; then ln -sf /usr/lib/systemd/system/sshd.service /etc/systemd/system/multi-user.target.wants/sshd.service; fi \
 && if [ -f /etc/ssh/sshd_config ]; then sed -i 's/^#\\?PermitRootLogin.*/PermitRootLogin yes/' /etc/ssh/sshd_config; fi \
 && if [ -f /etc/ssh/sshd_config ]; then sed -i 's/^#\\?PasswordAuthentication.*/PasswordAuthentication yes/' /etc/ssh/sshd_config; fi \
 && if [ -f /etc/ssh/sshd_config ]; then sed -i 's/^UsePAM.*/UsePAM no/' /etc/ssh/sshd_config; fi \
 && if [ -f /etc/ssh/sshd_config ] && ! grep -q '^UsePAM ' /etc/ssh/sshd_config; then echo 'UsePAM no' >> /etc/ssh/sshd_config; fi

RUN mkdir -p /var/lib/buckit-drives /data

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

STOPSIGNAL SIGRTMIN+3
ENTRYPOINT ["/entrypoint.sh"]
EOF
}
