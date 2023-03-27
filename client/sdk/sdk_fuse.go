// Copyright 2018 The CubeFS Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package main

//
// Usage: ./client -c fuse.json &
//
// Default mountpoint is specified in fuse.json, which is "/mnt".

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	syslog "log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	cfs "github.com/chubaofs/chubaofs/client/fs"
	"github.com/chubaofs/chubaofs/proto"
	"github.com/chubaofs/chubaofs/sdk/data"
	"github.com/chubaofs/chubaofs/sdk/master"
	"github.com/chubaofs/chubaofs/sdk/meta"
	"github.com/chubaofs/chubaofs/util/config"
	"github.com/chubaofs/chubaofs/util/errors"
	"github.com/chubaofs/chubaofs/util/log"
	sysutil "github.com/chubaofs/chubaofs/util/sys"
	"github.com/jacobsa/daemonize"

	"github.com/chubaofs/chubaofs/util/ump"
	"github.com/chubaofs/chubaofs/util/version"
)

const (
	MaxReadAhead = 512 * 1024

	defaultRlimit uint64 = 1024000
)

const (
	LoggerDir    = "client"
	LoggerPrefix = "client"
	LoggerOutput = "output.log"

	ConfigKeyExporterPort = "exporterKey"

	ControlCommandSetRate      = "/rate/set"
	ControlCommandGetRate      = "/rate/get"
	ControlCommandGetOpRate    = "/opRate/get"
	ControlCommandFreeOSMemory = "/debug/freeosmemory"
	ControlCommandTracing      = "/tracing"
	Role                       = "Client"

	StartRetryMaxCount    = 10
	StartRetryIntervalSec = 5
)

type fClient struct {
	configFile  string
	moduleName  string
	mountPoint  string
	stopC       chan struct{}
	super       *cfs.Super
	wg          sync.WaitGroup
	fuseServer  *fs.Server
	fsConn      *fuse.Conn
	clientState []byte
	outputFile  *os.File
	volName     string
	readonly    bool
	mc          *master.MasterClient
	mw          *meta.MetaWrapper
	ec          *data.ExtentClient
	stderrFd    int
	profPort    uint64
	portWg      sync.WaitGroup
}

type FuseClientState struct {
	FuseState  *fs.FuseContext
	MetaState  *meta.MetaState
	DataState  *data.DataState
	SuperState *cfs.SuperState
}

var GlobalMountOptions []proto.MountOption
var gClient *fClient
var fuseServerWg sync.WaitGroup

func init() {
	// add GODEBUG=madvdontneed=1 environ, to make sysUnused uses madvise(MADV_DONTNEED) to signal the kernel that a
	// range of allocated memory contains unneeded data.
	os.Setenv("GODEBUG", "madvdontneed=1")
	GlobalMountOptions = proto.NewMountOptions()
	proto.InitMountOptions(GlobalMountOptions)
}

func StartClient(configFile string, fuseFd *os.File, clientStateBytes []byte) (err error) {

	/*
	 * We are in daemon from here.
	 * Must notify the parent process through SignalOutcome anyway.
	 */
	cfg, _ := config.LoadConfigFile(configFile)
	opt, err := parseMountOption(cfg)
	if err != nil {
		return err
	}
	if opt.Modulename == "" {
		opt.Modulename = "fuseclient"
	}
	gClient = &fClient{
		configFile: configFile,
		moduleName: opt.Modulename,
		mountPoint: opt.MountPoint,
		stopC:      make(chan struct{}),
		volName:    opt.Volname,
		mc:         master.NewMasterClientFromString(opt.Master, false),
	}

	if opt.MaxCPUs > 0 {
		runtime.GOMAXPROCS(int(opt.MaxCPUs))
	} else {
		runtime.GOMAXPROCS(runtime.NumCPU())
	}

	level := parseLogLevel(opt.Loglvl)
	_, err = log.InitLog(opt.Logpath, opt.Volname, level, log.NewClientLogRotate())
	if err != nil {
		return err
	}

	outputFilePath := path.Join(opt.Logpath, opt.Volname, LoggerOutput)
	outputFile, err := os.OpenFile(outputFilePath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0666)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			syslog.Printf("start fuse client failed: err(%v)\n", err)
			err = fmt.Errorf("%v.\nPlease check %s for more details.", err, outputFilePath)
			outputFile.Sync()
			outputFile.Close()
		}
	}()
	gClient.outputFile = outputFile

	gClient.stderrFd, err = syscall.Dup(int(os.Stderr.Fd()))
	if err != nil {
		return err
	}
	if err = sysutil.RedirectFD(int(outputFile.Fd()), int(os.Stderr.Fd())); err != nil {
		return err
	}

	syslog.Println(dumpVersion())
	syslog.Println("*** Final Mount Options ***")
	for _, o := range GlobalMountOptions {
		syslog.Println(o)
	}
	syslog.Println("*** End ***")

	changeRlimit(defaultRlimit)

	registerInterceptedSignal()

	clientState := &FuseClientState{}
	first_start := clientStateBytes == nil
	if first_start {
		if err = lockPidFile(opt.PidFile); err != nil {
			syslog.Printf("lock pidFile %s failed: %v\n", opt.PidFile, err)
			log.LogFlush()
			return err
		}
		if err = checkMountPoint(opt.MountPoint); err != nil {
			syslog.Println("check MountPoint failed: ", err)
			log.LogFlush()
			return err
		}
		for retry := 0; retry < StartRetryMaxCount; retry++ {
			if err = checkPermission(opt); err == nil || err == proto.ErrNoPermission {
				break
			}
			log.LogWarnf("StartClient: checkPermission err(%v) retry count(%v)", err, retry)
			time.Sleep(StartRetryIntervalSec * time.Second)
		}
		if err != nil {
			syslog.Println("check permission failed: ", err)
			log.LogFlush()
			return err
		}
	} else {
		if err = json.Unmarshal(clientStateBytes, clientState); err != nil {
			syslog.Printf("Unmarshal clientState err: %v, clientState: %s\n", err, string(clientStateBytes))
			log.LogFlush()
			return err
		}
	}

	gClient.readonly = opt.Rdonly

	fsConn, err := mount(opt, fuseFd, first_start, clientState)
	if err != nil {
		syslog.Println("mount failed: ", err)
		log.LogFlush()
		return err
	}
	gClient.fsConn = fsConn

	// report client version
	var masters = strings.Split(opt.Master, meta.HostsSeparator)
	versionInfo := proto.DumpVersion(gClient.moduleName, BranchName, CommitID, BuildTime)
	gClient.wg.Add(2)
	go func() {
		gClient.portWg.Wait()
		version.ReportVersionSchedule(cfg, masters, versionInfo, gClient.volName, opt.MountPoint, CommitID, gClient.profPort, gClient.stopC, &gClient.wg)
	}()

	fuseServerWg.Add(1)
	go func() {
		defer func() {
			fuseServerWg.Done()
			gClient.wg.Done()
		}()
		var fuseState *fs.FuseContext
		if !first_start {
			fuseState = clientState.FuseState
		}
		gClient.fuseServer = fs.New(fsConn, nil)
		if fuseState, err = gClient.fuseServer.Serve(gClient.super, fuseState); err != nil {
			log.LogFlush()
			syslog.Printf("fs Serve returns err(%v)", err)
			os.Exit(1)
		}
		if fuseState == nil {
			log.LogFlush()
			os.Exit(0)
		}
		currState := FuseClientState{fuseState, gClient.mw.SaveMetaState(), gClient.ec.SaveDataState(), gClient.super.SaveSuperState()}
		state, err := json.Marshal(currState)
		if err != nil {
			syslog.Printf("Marshal clientState err(%v), clientState(%v)\n", err, currState)
			os.Exit(1)
		}
		gClient.clientState = state
	}()

	<-fsConn.Ready
	if fsConn.MountError != nil {
		log.LogFlush()
		syslog.Printf("fs Serve returns err(%v)\n", fsConn.MountError)
		return fsConn.MountError
	}
	return nil
}

func dumpVersion() string {
	return fmt.Sprintf("\nChubaoFS Client\nBranch: %s\nVersion: %s\nCommit: %s\nBuild: %s %s %s %s\n", BranchName, proto.Version, CommitID, runtime.Version(), runtime.GOOS, runtime.GOARCH, BuildTime)
}

func mount(opt *proto.MountOptions, fuseFd *os.File, first_start bool, clientState *FuseClientState) (fsConn *fuse.Conn, err error) {
	var super *cfs.Super
	if first_start {
		super, err = cfs.NewSuper(opt, first_start, nil, nil, nil)
	} else {
		super, err = cfs.NewSuper(opt, first_start, clientState.MetaState, clientState.DataState, clientState.SuperState)
	}
	if err != nil {
		log.LogError(errors.Stack(err))
		return
	}

	gClient.super = super
	gClient.mw = super.MetaWrapper()
	gClient.ec = super.ExtentClient()
	http.HandleFunc(ControlCommandSetRate, super.SetRate)
	http.HandleFunc(ControlCommandGetRate, super.GetRate)
	http.HandleFunc(ControlCommandGetOpRate, super.GetOpRate)
	http.HandleFunc(ControlCommandFreeOSMemory, freeOSMemory)
	http.HandleFunc(ControlVersion, GetVersionHandleFunc)
	http.HandleFunc(ControlSetUpgrade, gClient.SetClientUpgrade)
	http.HandleFunc(ControlUnsetUpgrade, UnsetClientUpgrade)
	http.HandleFunc(ControlCommandGetUmpCollectWay, GetUmpCollectWay)
	http.HandleFunc(ControlCommandSetUmpCollectWay, SetUmpCollectWay)
	http.HandleFunc(ControlAccessRoot, gClient.AccessRoot)
	var (
		server *http.Server
		lc     net.ListenConfig
	)

	gClient.wg.Add(2)
	gClient.portWg.Add(1)
	go func() {
		defer gClient.wg.Done()
		defer func() {
			gClient.profPort = 0
		}()
		if opt.Profport != "" {
			syslog.Println("Start pprof with port:", opt.Profport)
			server = &http.Server{Addr: fmt.Sprintf(":%v", opt.Profport)}
			ln, err := lc.Listen(context.Background(), "tcp", server.Addr)
			if err == nil {
				gClient.profPort, _ = strconv.ParseUint(opt.Profport, 10, 64)
				gClient.portWg.Done()
				if err = server.Serve(ln); err != http.ErrServerClosed {
					syslog.Printf("Start with config pprof[%v] failed, err: %v", gClient.profPort, err)
				}
				return
			}
		}

		syslog.Printf("Start with config pprof[%v] failed, try %v to %v\n", opt.Profport, log.DefaultProfPort,
			log.MaxProfPort)

		for port := log.DefaultProfPort; port <= log.MaxProfPort; port++ {
			syslog.Println("Start pprof with port:", port)
			server = &http.Server{Addr: fmt.Sprintf(":%v", strconv.Itoa(port))}
			ln, err := lc.Listen(context.Background(), "tcp", server.Addr)
			if err != nil {
				syslog.Println("Start pprof err: ", err)
				continue
			}
			gClient.profPort = uint64(port)
			gClient.portWg.Done()
			if err = server.Serve(ln); err != http.ErrServerClosed {
				syslog.Printf("Start with config pprof[%v] failed, err: %v", gClient.profPort, err)
			}
			return
		}
		gClient.profPort = 0
		gClient.portWg.Done()
	}()

	go func() {
		defer gClient.wg.Done()
		<-gClient.stopC
		server.Shutdown(context.Background())
	}()

	ump.SetUmpJmtpAddr(super.UmpJmtpAddr())
	ump.SetUmpCollectWay(proto.UmpCollectBy(opt.UmpCollectWay))
	if err = ump.InitUmp(fmt.Sprintf("%v_%v_%v", super.ClusterName(), super.VolName(), gClient.moduleName), "jdos_chubaofs_node"); err != nil {
		return
	}

	options := []fuse.MountOption{
		fuse.AllowOther(),
		fuse.MaxReadahead(uint32(opt.ReadAheadSize)),
		fuse.AsyncRead(),
		fuse.AutoInvalData(opt.AutoInvalData),
		fuse.FSName("chubaofs-" + opt.Volname),
		fuse.LocalVolume(),
		fuse.VolumeName("chubaofs-" + opt.Volname)}

	if opt.Rdonly {
		options = append(options, fuse.ReadOnly())
	}

	if opt.WriteCache {
		options = append(options, fuse.WritebackCache())
		log.LogInfof("mount: vol enable write cache(%v)", opt.WriteCache)
		super.SetEnableWriteCache(true)
		if super.SupportJdosKernelWriteBack() {
			options = append(options, fuse.WritebackCacheForCGroup())
		}
	}

	if opt.EnablePosixACL {
		options = append(options, fuse.PosixACL())
	}

	fsConn, err = fuse.Mount(opt.MountPoint, fuseFd, options...)
	return
}

func registerInterceptedSignal() {
	sigC := make(chan os.Signal, 1)
	signal.Notify(sigC, syscall.SIGINT, syscall.SIGTERM, syscall.SIGPIPE, syscall.SIGUSR1, syscall.SIGUSR2, syscall.SIGALRM, syscall.SIGXCPU, syscall.SIGXFSZ, syscall.SIGVTALRM, syscall.SIGPROF, syscall.SIGIO, syscall.SIGPWR)
	gClient.wg.Add(1)
	go func() {
		defer gClient.wg.Done()
		select {
		case <-gClient.stopC:
			return
		case sig := <-sigC:
			if sig == syscall.SIGINT || sig == syscall.SIGTERM {
				syslog.Printf("Killed due to a received signal (%v)\n", sig)
				gClient.outputFile.Sync()
				gClient.outputFile.Close()
				os.Exit(1)
			}
		}
	}()
}

func parseMountOption(cfg *config.Config) (*proto.MountOptions, error) {
	var err error
	opt := new(proto.MountOptions)

	proto.ParseMountOptions(GlobalMountOptions, cfg)

	opt.MountPoint = GlobalMountOptions[proto.MountPoint].GetString()
	opt.Modulename = GlobalMountOptions[proto.Modulename].GetString()
	opt.Volname = GlobalMountOptions[proto.VolName].GetString()
	opt.Owner = GlobalMountOptions[proto.Owner].GetString()
	opt.Master = GlobalMountOptions[proto.Master].GetString()
	opt.Logpath = GlobalMountOptions[proto.LogDir].GetString()
	opt.Loglvl = GlobalMountOptions[proto.LogLevel].GetString()
	opt.Profport = GlobalMountOptions[proto.ProfPort].GetString()
	opt.IcacheTimeout = GlobalMountOptions[proto.IcacheTimeout].GetInt64()
	opt.LookupValid = GlobalMountOptions[proto.LookupValid].GetInt64()
	opt.AttrValid = GlobalMountOptions[proto.AttrValid].GetInt64()
	opt.ReadRate = GlobalMountOptions[proto.ReadRate].GetInt64()
	opt.WriteRate = GlobalMountOptions[proto.WriteRate].GetInt64()
	opt.EnSyncWrite = GlobalMountOptions[proto.EnSyncWrite].GetInt64()
	opt.AutoInvalData = GlobalMountOptions[proto.AutoInvalData].GetInt64()
	opt.Rdonly = GlobalMountOptions[proto.Rdonly].GetBool()
	opt.WriteCache = GlobalMountOptions[proto.WriteCache].GetBool()
	opt.KeepCache = GlobalMountOptions[proto.KeepCache].GetBool()
	opt.FollowerRead = GlobalMountOptions[proto.FollowerRead].GetBool()
	opt.Authenticate = GlobalMountOptions[proto.Authenticate].GetBool()
	if opt.Authenticate {
		opt.TicketMess.ClientKey = GlobalMountOptions[proto.ClientKey].GetString()
		ticketHostConfig := GlobalMountOptions[proto.TicketHost].GetString()
		ticketHosts := strings.Split(ticketHostConfig, ",")
		opt.TicketMess.TicketHosts = ticketHosts
		opt.TicketMess.EnableHTTPS = GlobalMountOptions[proto.EnableHTTPS].GetBool()
		if opt.TicketMess.EnableHTTPS {
			opt.TicketMess.CertFile = GlobalMountOptions[proto.CertFile].GetString()
		}
	}
	opt.TokenKey = GlobalMountOptions[proto.TokenKey].GetString()
	opt.AccessKey = GlobalMountOptions[proto.AccessKey].GetString()
	opt.SecretKey = GlobalMountOptions[proto.SecretKey].GetString()
	opt.DisableDcache = GlobalMountOptions[proto.DisableDcache].GetBool()
	opt.SubDir = GlobalMountOptions[proto.SubDir].GetString()
	opt.AutoMakeSubDir = GlobalMountOptions[proto.AutoMakeSubDir].GetBool()
	opt.FsyncOnClose = GlobalMountOptions[proto.FsyncOnClose].GetBool()
	opt.MaxCPUs = GlobalMountOptions[proto.MaxCPUs].GetInt64()
	opt.EnableXattr = GlobalMountOptions[proto.EnableXattr].GetBool()
	opt.NearRead = GlobalMountOptions[proto.NearRead].GetBool()
	//opt.AlignSize = GlobalMountOptions[proto.AlignSize].GetInt64()
	//opt.MaxExtentNumPerAlignArea = GlobalMountOptions[proto.MaxExtentNumPerAlignArea].GetInt64()
	//opt.ForceAlignMerge = GlobalMountOptions[proto.ForceAlignMerge].GetBool()
	opt.EnablePosixACL = GlobalMountOptions[proto.EnablePosixACL].GetBool()
	opt.ExtentSize = GlobalMountOptions[proto.ExtentSize].GetInt64()
	opt.AutoFlush = GlobalMountOptions[proto.AutoFlush].GetBool()
	opt.DelProcessPath = GlobalMountOptions[proto.DeleteProcessAbsoPath].GetString()
	opt.NoBatchGetInodeOnReaddir = GlobalMountOptions[proto.NoBatchGetInodeOnReaddir].GetBool()
	opt.ReadAheadSize = GlobalMountOptions[proto.ReadAheadSize].GetInt64()
	if opt.ReadAheadSize > MaxReadAhead || opt.ReadAheadSize < 0 || opt.ReadAheadSize%4096 != 0 {
		return nil, errors.New(fmt.Sprintf("the size of kernel read-ahead ranges from 0~512KB and must be divisible by 4096, invalid value: %v", opt.ReadAheadSize))
	}

	if opt.MountPoint == "" || opt.Volname == "" || opt.Owner == "" || opt.Master == "" {
		return nil, errors.New(fmt.Sprintf("invalid config file: lack of mandatory fields, mountPoint(%v), volName(%v), owner(%v), masterAddr(%v)", opt.MountPoint, opt.Volname, opt.Owner, opt.Master))
	}

	absMnt, err := filepath.Abs(opt.MountPoint)
	if err != nil {
		return nil, errors.Trace(err, "invalide mount point (%v) ", opt.MountPoint)
	}
	opt.MountPoint = absMnt
	collectWay := GlobalMountOptions[proto.UmpCollectWay].GetInt64()
	if collectWay <= proto.UmpCollectByUnkown || collectWay > proto.UmpCollectByJmtpClient {
		collectWay = proto.UmpCollectByFile
	}
	opt.UmpCollectWay = collectWay
	opt.PidFile = GlobalMountOptions[proto.PidFile].GetString()
	if opt.PidFile != "" && opt.PidFile[0] != os.PathSeparator {
		return nil, fmt.Errorf("invalid config file: pidFile(%s) must be a absolute path", opt.PidFile)
	}
	return opt, nil
}

func checkMountPoint(mountPoint string) error {
	if mountPoint == "/" {
		return fmt.Errorf("Multiple mount are not supported: %s", mountPoint)
	}
	stat, err := os.Stat(mountPoint)
	if err != nil {
		return err
	}
	rootStat, err := os.Stat(filepath.Dir(strings.TrimSuffix(mountPoint, "/")))
	if err != nil {
		return err
	}
	if stat.Sys().(*syscall.Stat_t).Dev != rootStat.Sys().(*syscall.Stat_t).Dev {
		return fmt.Errorf("Multiple mount are not supported: %s", mountPoint)
	}
	return nil
}

func checkPermission(opt *proto.MountOptions) (err error) {
	// Check token permission
	var info *proto.VolStatInfo
	if info, err = gClient.mc.ClientAPI().GetVolumeStat(opt.Volname); err != nil {
		err = errors.Trace(err, "Get volume stat failed, check your masterAddr!")
		return
	}
	if info.EnableToken {
		var token *proto.Token
		if token, err = gClient.mc.ClientAPI().GetToken(opt.Volname, opt.TokenKey); err != nil {
			log.LogWarnf("checkPermission: get token type failed: volume(%v) tokenKey(%v) err(%v)",
				opt.Volname, opt.TokenKey, err)
			return
		}
		log.LogInfof("checkPermission: get token: token(%v)", token)
		opt.Rdonly = token.TokenType == int8(proto.ReadOnlyToken) || opt.Rdonly
	}

	// Get write-cache options
	opt.WriteCache = info.EnableWriteCache && opt.WriteCache

	// Check user access policy is enabled
	if opt.AccessKey != "" {
		var userInfo *proto.UserInfo
		if userInfo, err = gClient.mc.UserAPI().GetAKInfo(opt.AccessKey); err != nil {
			return
		}
		if userInfo.SecretKey != opt.SecretKey {
			err = proto.ErrNoPermission
			return
		}
		var policy = userInfo.Policy
		if policy.IsOwn(opt.Volname) {
			return
		}
		if policy.IsAuthorized(opt.Volname, "", proto.POSIXWriteAction) &&
			policy.IsAuthorized(opt.Volname, "", proto.POSIXReadAction) {
			return
		}
		if policy.IsAuthorized(opt.Volname, "", proto.POSIXReadAction) &&
			!policy.IsAuthorized(opt.Volname, "", proto.POSIXWriteAction) {
			opt.Rdonly = true
			return
		}
		err = proto.ErrNoPermission
		return
	}
	return
}

func parseLogLevel(loglvl string) log.Level {
	var level log.Level
	switch strings.ToLower(loglvl) {
	case "debug":
		level = log.DebugLevel
	case "info":
		level = log.InfoLevel
	case "warn":
		level = log.WarnLevel
	case "error":
		level = log.ErrorLevel
	default:
		level = log.ErrorLevel
	}
	return level
}

func changeRlimit(val uint64) {
	rlimit := &syscall.Rlimit{Max: val, Cur: val}
	err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, rlimit)
	if err != nil {
		syslog.Printf("Failed to set rlimit to %v \n", val)
	} else {
		syslog.Printf("Successfully set rlimit to %v \n", val)
	}
}

func freeOSMemory(w http.ResponseWriter, r *http.Request) {
	debug.FreeOSMemory()
}

func GetFuseFd() *os.File {
	return gClient.fsConn.Fusefd()
}

func StopClient() (clientState []byte) {
	gClient.fuseServer.Stop()
	close(gClient.stopC)
	gClient.wg.Wait()
	clientState = gClient.clientState

	gClient.super.Close()
	syslog.Println("Stop fuse client successfully.")

	sysutil.RedirectFD(gClient.stderrFd, int(os.Stderr.Fd()))
	gClient.outputFile.Sync()
	gClient.outputFile.Close()

	ump.StopUmp()
	log.LogClose()
	gClient = nil

	runtime.GC()
	return
}

func GetVersion() string {
	return dumpVersion()
}

func startDaemon(file string) error {
	cmdPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("startDaemon failed: cannot get absolute command path, err(%v)", err)
	}

	if len(os.Args) <= 1 {
		return fmt.Errorf("startDaemon failed: cannot use null arguments")
	}

	args := []string{"-f"}
	args = append(args, os.Args[1:]...)

	if file != "" {
		configPath, err := filepath.Abs(file)
		if err != nil {
			return fmt.Errorf("startDaemon failed: cannot get absolute command path of config file(%v) , err(%v)", file, err)
		}
		for i := 0; i < len(args); i++ {
			if args[i] == "-c" {
				// Since file is not "", the (i+1)th argument must be the config file path
				args[i+1] = configPath
				break
			}
		}
	}

	env := os.Environ()
	buf := new(bytes.Buffer)
	err = daemonize.Run(cmdPath, args, env, buf)
	if err != nil {
		if buf.Len() > 0 {
			fmt.Println(buf.String())
		}
		return fmt.Errorf("startDaemon failed.\ncmd(%v)\nargs(%v)\nerr(%v)\n", cmdPath, args, err)
	}
	return nil
}

func main() {
	var (
		configFile       = flag.String("c", "", "FUSE client config file")
		configForeground = flag.Bool("f", false, "run foreground")
		configVersion    = flag.Bool("v", false, "Show client version")
	)
	flag.Parse()

	if !*configVersion && *configFile == "" {
		fmt.Printf("Usage: %s -c {configFile}\n", os.Args[0])
		os.Exit(1)
	}
	var err error

	if *configVersion {
		*configForeground = true
	}
	if !*configForeground {
		if err := startDaemon(*configFile); err != nil {
			fmt.Printf("%s\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if *configVersion {
		fmt.Println(GetVersion())
		os.Exit(0)
	}
	err = StartClient(*configFile, nil, nil)
	if err != nil {
		fmt.Printf("\nStart fuse client failed: %v\n", err.Error())
		_ = daemonize.SignalOutcome(err)
		os.Exit(1)
	} else {
		_ = daemonize.SignalOutcome(nil)
	}
	fuseServerWg.Wait()
}
