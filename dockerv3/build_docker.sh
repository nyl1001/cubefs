#!/usr/bin/env bash

RootPath=$(cd $(dirname $0) ; pwd)
CfsBase="cubefs/cbfs-base:1.0-golang-1.18.10"

docker build -t ${CfsBase} -f ${RootPath}/Dockerfile ${RootPath}
