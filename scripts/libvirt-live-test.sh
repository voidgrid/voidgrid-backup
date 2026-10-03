#!/bin/sh
# Live VM backup test against the per-user qemu:///session libvirt (never
# the system daemon, so real VMs are neither touched nor visible). Creates a
# small empty qcow2 under ./.cache, runs the test in a Go container with the
# session socket and the repo mounted at their host paths, and removes the
# disk afterwards. The test itself starts and destroys a transient domain.
set -eu
cd "$(dirname "$0")/.."
mkdir -p .cache/gopath .cache/gobuild .cache/vmtest
run="/run/user/$(id -u)/libvirt"
virsh -q -c qemu:///session uri >/dev/null # socket-activates the session daemon
sock="$run/virtqemud-sock"
[ -S "$sock" ] || sock="$run/libvirt-sock"
disk="$PWD/.cache/vmtest/disk-$$.qcow2"
name="vb-live-$$"
trap 'virsh -q -c qemu:///session destroy "$name" >/dev/null 2>&1 || true; rm -f "$disk" "$disk".vb-*' EXIT
qemu-img create -q -f qcow2 "$disk" 32M
docker run --rm -i \
  --user "$(id -u):$(id -g)" \
  -v "$run:$run" \
  -v "$PWD:$PWD" -w "$PWD" \
  -v "$PWD/.cache/gopath:/gopath" \
  -v "$PWD/.cache/gobuild:/tmp/gobuild" \
  -e GOPATH=/gopath -e GOCACHE=/tmp/gobuild -e GOFLAGS=-buildvcs=false -e HOME=/tmp \
  -e VB_LIBVIRT_SOCK="$sock" -e VB_VM_DISK="$disk" -e VB_VM_NAME="$name" \
  golang:1.27 go test -count=1 -tags libvirtlive -run TestLiveVMBackup -v ./internal/engine/
