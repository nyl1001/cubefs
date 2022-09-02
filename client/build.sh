#!/usr/bin/env bash

BranchName=`git rev-parse --abbrev-ref HEAD`
CommitID=`git rev-parse HEAD`
BuildTime=`date +%Y-%m-%d\ %H:%M`
Debug="0"

[[ "-$GOPATH" == "-" ]] && { echo "GOPATH not set"; exit 1 ; }

goflag="-s"

build_sdk=1
build_client=1
build_test=0
pack_libs=0

help() {
    cat <<EOF

Usage: ./build.sh [ -h | --help ] [ -g ] [ --sdk-only | --client-only ]
    -h, --help              show help info
    -g                      setup Debug="1" goflag="" gccflag="-g"
    -s, --sdk-only              build sdk (libcfssdk.so libempty.so) only
    -c, --client-only           build client (libcfsclient.so and cfs-client) only
    -p, --pack-libs             pack libs to cfs-client-libs.tar.gz used for bypass upgrade
    test                    build in test mode
EOF
    exit 0
}

ARGS=( "$@" )
for opt in ${ARGS[*]} ; do
    case "$opt" in
        -h | --help)
            help
            ;;
        -g)
            Debug="1"
            goflag=""
            gccflag="-g"
            ;;
    	-s | --sdk-only)
    	    build_sdk=1
    	    build_client=0
	        ;;
	    -c | --client-only)
	        build_sdk=0
	        build_client=1
	        ;;
        -p | --pack-libs)
            build_sdk=1
            build_client=1
            pack_libs=1
            ;;
        test)
            build_test=1
            build_sdk=1
            build_client=1
            ;;
    esac
done

dir=$(dirname $0)
bin=${dir}/bin
echo "using Debug=\"${Debug}\""
echo "using goflag=\"${goflag}\""
echo "using gccflag=\"${gccflag}\""
if [[ ${build_sdk} -eq 1 ]]; then
    echo "building sdk (libcfssdk.so, libcfssdk_cshared.so) commit: ${CommitID} ..."
    go build -ldflags "${goflag} -E main.main -X main.BranchName=${BranchName} -X main.CommitID=${CommitID} -X 'main.BuildTime=${BuildTime}' -X 'main.Debug=${Debug}'" -buildmode=plugin -linkshared -o ${bin}/libcfssdk.so ${dir}/sdk_fuse.go ${dir}/sdk_bypass.go ${dir}/http.go ${dir}/ump.go
    go build -ldflags "${goflag} -X main.CommitID=${CommitID} -X main.BranchName=${BranchName} -X 'main.BuildTime=${BuildTime}' -X 'main.Debug=${Debug}'" -buildmode=c-shared -o ${bin}/libcfssdk_cshared.so ${dir}/sdk_fuse.go ${dir}/sdk_bypass.go ${dir}/http.go ${dir}/ump.go
    chmod a+rx ${bin}/libcfssdk.so ${bin}/libcfssdk_cshared.so
fi
if [[ ${build_client} -eq 1 ]]; then
    echo "building client (cfs-client libcfsclient.so libempty.so libcfsc.so) ..."
    go build -ldflags "${goflag}" -buildmode=plugin -linkshared -o ${bin}/libempty.so  ${dir}/empty.go
    go build -ldflags "${goflag}" -linkshared -o ${bin}/cfs-client ${dir}/main_fuse.go
    gcc ${gccflag} -std=c99 -fPIC -shared -o ${bin}/libcfsclient.so ${dir}/main_hook.c ${dir}/bypass/libc_operation.c -ldl -lpthread -I ${dir}/bypass/include
    g++ -std=c++11 ${gccflag} -DCommitID=\"${CommitID}\" -fPIC -shared -o ${bin}/libcfsc.so ${dir}/bypass/client.c ${dir}/bypass/cache.c ${dir}/bypass/packet.c ${dir}/bypass/conn_pool.c ${dir}/bypass/ini.c ${dir}/bypass/libc_operation.c -ldl -lpthread -I ${dir}/bypass/include
    chmod a+rx ${bin}/libempty.so ${bin}/cfs-client ${bin}/libcfsclient.so ${bin}/libcfsc.so
fi
if [[ ${build_test} -eq 1 ]]; then
    echo "building test (cfs-client test-bypass libcfsclient.so libempty.so) ..."
    go test -c -covermode=atomic -coverpkg="../..." -linkshared -o ${bin}/cfs-client ${dir}/main_fuse.go ${dir}/fuse_test.go
    gcc ${dir}/bypass/client_test.c -o ${bin}/test-bypass
fi
if [[ ${pack_libs} -eq 1 ]]; then
    echo "pack libs, generate cfs-client-libs.tar.gz ..."
    cd ${bin}
    md5sum libcfssdk.so > checkfile
    md5sum libcfsc.so >> checkfile
    tar -zcvf cfs-client-libs_${CommitID}.tar.gz  libcfssdk.so libcfsc.so checkfile
    cd ~-
fi
