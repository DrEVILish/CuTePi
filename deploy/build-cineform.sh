#!/bin/sh
# Builds and installs the CineForm SDK library CuTePi's CineForm decoder loads at run time
# (/usr/local/lib/cutepi/libcutepi-cfhd.so). Needs a C/C++ compiler and uuid-dev; about 2 minutes on a Pi 4.
set -e
cd "$(dirname "$0")/../third_party/cineform-sdk"
make -j"$(nproc)"
make install
