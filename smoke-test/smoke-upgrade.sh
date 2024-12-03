#!/usr/bin/env bash

K0SCTL_CONFIG=${K0SCTL_CONFIG:-"k0sctl.yaml"}

set -e

. ./smoke.common.sh
trap cleanup EXIT


deleteCluster
createCluster

export EXTERNAL_ADDRESS=$(externalAddress)

remoteCommand() {
  local userhost="$1"
  shift
  echo "* Running command on ${userhost}: $*"
  bootloose ssh "${userhost}" -- "$*"
}

# Create config with older version and apply
K0S_VERSION="${K0S_FROM}"
echo "Installing ${K0S_VERSION}"
../k0sctl apply --config "${K0SCTL_CONFIG}" --debug
remoteCommand "root@manager0" "k0s version | grep -q ${K0S_FROM}"

K0S_VERSION=$(curl -s "https://docs.k0sproject.io/stable.txt")

# Create config with latest version and apply as upgrade
echo "Upgrading to k0s ${K0S_VERSION}"
../k0sctl apply --config "${K0SCTL_CONFIG}" --debug
remoteCommand "root@manager0" "k0s version | grep -q ${K0S_VERSION}"
