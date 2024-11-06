#!/usr/bin/env bash
set -Eeuo pipefail

cd ../plgo
go build -o $(go env GOPATH)/bin/plgo .
cd ../test
plgo
cd build
sudo make install with_llvm=no
cd ..
sudo -u postgres psql -c "drop extension if exists test" postgres
sudo -u postgres psql -c "create extension test" postgres
sudo -u postgres psql -c "select plgotest()" postgres

