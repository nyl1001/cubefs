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

import (
	"flag"
	"fmt"
	"github.com/cubefs/cubefs/sdk/convert"
	"os"

	"github.com/cubefs/cubefs/cli/cmd"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/master"
	"github.com/cubefs/cubefs/util/log"
	"github.com/spf13/cobra"
)

var (
	CommitID   string
	BranchName string
	BuildTime  string
)

func runCLI() (err error) {
	var cfg *cmd.Config
	defer log.LogFlush()
	if cfg, err = cmd.LoadConfig(); err != nil {
		return
	}
	if cfg.IsDbBack == 1 {
		proto.IsDbBack = true
	}
	cfscli := setupCommands(cfg)
	err = cfscli.Execute()
	return
}

func setupCommands(cfg *cmd.Config) *cobra.Command {
	var mc = master.NewMasterClientWithoutTimeout(cfg.MasterAddr, false)
	mc.DataNodeProfPort = cfg.DataNodeProfPort
	mc.MetaNodeProfPort = cfg.MetaNodeProfPort
	mc.EcNodeProfPort = cfg.EcNodeProfPort
	mc.FlashNodeProfPort = cfg.FlashNodeProfPort
	cc := convert.NewConvertClient(cfg.ConvertAddr, false)
	cfsRootCmd := cmd.NewRootCmd(mc, cc)
	var completionCmd = &cobra.Command{
		Use:   "completion",
		Short: "Generate completion bash file",
		Long: `To use the completion, tool "bash-completion" is demanded,
then execute the following command:
   $ ./cfs-cli completion   
   # Bash completion file "cfs-cli.sh" will be generated under the present working directory
   $ echo 'source /usr/share/bash-completion/bash_completion' >> ~/.bashrc
   $ echo 'source {path of cfs-cli.sh}' >>~/.bashrc
   $ source ~/.bashrc
`,
		Example: "cfs-cli completion",
		Run: func(cmd *cobra.Command, args []string) {
			if err := cfsRootCmd.CFSCmd.GenBashCompletionFile("cfs-cli.sh"); err != nil {
				_, _ = fmt.Fprintf(os.Stdout, "generate bash file failed")
			}
			_, _ = fmt.Fprintf(os.Stdout, `File "cfs-cli.sh" has been generated successfully under the present working directory,
following command to execute:
   $ # install bash-completion 
   $ echo 'source /usr/share/bash-completion/bash_completion' >> ~/.bashrc
   $ echo 'source {path of cfs-cli.sh}' >>~/.bashrc
   $ source ~/.bashrc
`)
		},
	}
	cfsRootCmd.CFSCmd.AddCommand(completionCmd)
	return cfsRootCmd.CFSCmd
}

var configVersion = flag.Bool("v", false, "Show client version")

func main() {
	flag.Parse()
	var err error
	_, err = log.InitLog("/tmp/cfs", "cli", log.DebugLevel, nil)
	if err != nil {
		fmt.Printf(err.Error())
		os.Exit(1)
	}
	Version := proto.DumpVersion("ClI", BranchName, CommitID, BuildTime)
	if *configVersion {
		fmt.Printf("%v", Version)
		return
	}
	defer log.LogFlush()
	if err = runCLI(); err != nil {
		log.LogFlush()
		_, _ = fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
