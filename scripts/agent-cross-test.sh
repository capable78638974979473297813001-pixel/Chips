#!/usr/bin/env bash
# Builds chipagent for other CPU architectures and runs its test suite under
# QEMU user-mode emulation. Needs the matching cross compilers and qemu-user:
#   apt-get install qemu-user g++-aarch64-linux-gnu g++-arm-linux-gnueabihf \
#     g++-i686-linux-gnu g++-powerpc64le-linux-gnu g++-s390x-linux-gnu \
#     g++-riscv64-linux-gnu g++-mips-linux-gnu
# Usage: scripts/agent-cross-test.sh [triple...]   (default: all below)
set -euo pipefail

cd "$(dirname "$0")/.."
# triple:qemu-binary. Covers 32/64-bit, little/big-endian, and five ISA families.
all=(
  aarch64-linux-gnu:qemu-aarch64        # 64-bit Arm: Graviton, Grace, Ampere, Jetson, Raspberry Pi 4/5
  arm-linux-gnueabihf:qemu-arm          # 32-bit Arm: older Raspberry Pi and embedded boards
  i686-linux-gnu:qemu-i386              # 32-bit x86: Pentium-era PCs
  powerpc64le-linux-gnu:qemu-ppc64le    # IBM POWER8/9/10
  s390x-linux-gnu:qemu-s390x            # IBM Z mainframes (big-endian)
  riscv64-linux-gnu:qemu-riscv64        # RISC-V
  mips-linux-gnu:qemu-mips              # 32-bit MIPS (big-endian): routers, embedded
)
targets=("$@")
[ ${#targets[@]} -eq 0 ] && targets=("${all[@]%%:*}")

status=0
for entry in "${all[@]}"; do
  triple=${entry%%:*}
  qemu=${entry##*:}
  [[ " ${targets[*]} " == *" $triple "* ]] || continue
  build=agent/build-$triple
  echo "=== $triple"
  cmake -S agent -B "$build" -DCMAKE_BUILD_TYPE=Release -DCMAKE_COMPILE_WARNING_AS_ERROR=ON \
    -DCMAKE_SYSTEM_NAME=Linux -DCMAKE_SYSTEM_PROCESSOR="${triple%%-*}" \
    -DCMAKE_C_COMPILER="$triple-gcc" -DCMAKE_CXX_COMPILER="$triple-g++" >/dev/null
  cmake --build "$build" -j >/dev/null
  if QEMU_LD_PREFIX="/usr/$triple" "$qemu" "$build/chipagent_test"; then
    QEMU_LD_PREFIX="/usr/$triple" "$qemu" "$build/chipagent" --once | head -c 200
    echo
  else
    echo "FAILED: $triple"
    status=1
  fi
done
exit $status
