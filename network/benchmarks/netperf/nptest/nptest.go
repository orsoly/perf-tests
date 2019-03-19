/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

/*
 nptest.go

 Dual-mode program - runs as both the orchestrator and as the worker nodes depending on command line flags
 The RPC API is contained wholly within this file.
*/

package main

// Imports only base Golang packages
import (
	"bytes"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/rpc"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
	"time"
)

type point struct {
	_type  	   int
	key        string
	value	   string
}

var mode string
var port string
var host string
var worker string
var kubenode string
var podname string
var primaryNodeIP string
var secondaryNodeIP string
var nodePort string
var mssStepSize int

var workerStateMap map[string]*workerState

var iperfTCPOutputRegexp *regexp.Regexp
var iperfUDPOutputRegexp *regexp.Regexp
var netperfOutputRegexp *regexp.Regexp
var iperfCPUOutputRegexp *regexp.Regexp
var fortioOutputRegexp *regexp.Regexp
var pingOutputRegexp *regexp.Regexp

var dataPoints map[string][]point
var dataPointKeys []string
var dataPointLabels map[string]string
var datapointsFlushed bool

var globalLock sync.Mutex

const (
	workerMode           = "worker"
	orchestratorMode     = "orchestrator"
	iperf3Path           = "/usr/bin/iperf3"
	netperfPath          = "/usr/local/bin/netperf"
	netperfServerPath    = "/usr/local/bin/netserver"
	fortioPath           = "/usr/bin/fortio"
	outputCaptureFile    = "/tmp/output.txt"
	mssMin               = 96
	mssMax               = 1460
	parallelStreams      = "8"
	rpcServicePort       = "5202"
	localhostIPv4Address = "127.0.0.1"
)

const (
	iperfTcpTest = iota
	iperfUdpTest = iota
	netperfTest  = iota
	fortioTest   = iota
	pingTest   = iota
)

// NetPerfRpc service that exposes RegisterClient and ReceiveOutput for clients
type NetPerfRpc int

// ClientRegistrationData stores a data about a single client
type ClientRegistrationData struct {
	Host            string
	KubeNode        string
	Worker          string
	IP              string
	PrimaryNodeIP   string
	SecondaryNodeIP string
	NodePort        string
}

// IperfClientWorkItem represents a single task for an Iperf client
type IperfClientWorkItem struct {
	Host string
	Port string
	MSS  int
	Type int
}

// IperfServerWorkItem represents a single task for an Iperf server
type IperfServerWorkItem struct {
	ListenPort string
	Timeout    int
}

// WorkItem represents a single task for a worker
type WorkItem struct {
	IsClientItem bool
	IsServerItem bool
	IsIdle       bool
	ClientItem   IperfClientWorkItem
	ServerItem   IperfServerWorkItem
}

type workerState struct {
	sentServerItem  bool
	idle            bool
	IP              string
	worker          string
	primaryNodeIP   string
	secondaryNodeIP string
	nodePort        string
}

// WorkerOutput stores the results from a single worker
type WorkerOutput struct {
	Output string
	Code   int
	Worker string
	Type   int
}

type testcase struct {
	SourceNode       string
	DestinationNode  string
	Index			 string
	Label            string
	ClusterIP        bool
	Finished         bool
	MSS              int
	Type             int
	UsePrimaryNodeIP bool
}

var testcases []*testcase
var currentJobIndex int

func init() {
	flag.StringVar(&mode, "mode", "worker", "Mode for the daemon (worker | orchestrator)")
	flag.StringVar(&port, "port", rpcServicePort, "Port to listen on (defaults to 5202)")
	flag.StringVar(&host, "host", "", "IP address to bind to (defaults to 0.0.0.0)")

	var err error

        s := os.Getenv("mssStepSize")
        mssStepSize, err = strconv.Atoi(s)
        if err != nil {
				fmt.Println(err)
        }

	workerStateMap = make(map[string]*workerState)
	testcases = []*testcase{
		{SourceNode: "netperf-w1", DestinationNode: "netperf-w2", Index: "1", Label:"iperf TCP. Same VM using Pod IP", Type: iperfTcpTest, ClusterIP: false, MSS: mssMin},
		{SourceNode: "netperf-w1", DestinationNode: "netperf-w2", Index: "2", Label:"iperf TCP. Same VM using Virtual IP", Type: iperfTcpTest, ClusterIP: true, MSS: mssMin},
		//{SourceNode: "netperf-w1", DestinationNode: "netperf-w3", Index: "3", Label:"iperf TCP. Remote VM using Pod IP", Type: iperfTcpTest, ClusterIP: false, MSS: mssMin},
		//{SourceNode: "netperf-w3", DestinationNode: "netperf-w2", Index: "4", Label:"iperf TCP. Remote VM using Virtual IP", Type: iperfTcpTest, ClusterIP: true, MSS: mssMin},
		{SourceNode: "netperf-w2", DestinationNode: "netperf-w2", Index: "5", Label:"iperf TCP. Hairpin Pod to own Virtual IP", Type: iperfTcpTest, ClusterIP: true, MSS: mssMin},
		{SourceNode: "netperf-w1", DestinationNode: "netperf-w4", Index: "6", Label:"iperf TCP. Same VM, primary node IP using NodePort", Type: iperfTcpTest, ClusterIP: false, UsePrimaryNodeIP: true,  MSS: mssMin},
		{SourceNode: "netperf-w1", DestinationNode: "netperf-w4", Index: "7", Label:"iperf TCP. Same VM, secondary node IP using NodePort", Type: iperfTcpTest, ClusterIP: false, UsePrimaryNodeIP: false, MSS: mssMin},
		//{SourceNode: "netperf-w3", DestinationNode: "netperf-w4", Index: "8", Label:"iperf TCP. Remote VM, primary node IP using NodePort", Type: iperfTcpTest, ClusterIP: false, UsePrimaryNodeIP: true, MSS: mssMin},
		{SourceNode: "netperf-w1", DestinationNode: "netperf-w2", Index: "9", Label:"iperf UDP. Same VM using Pod IP", Type: iperfUdpTest, ClusterIP: false, MSS: mssMax},
		{SourceNode: "netperf-w1", DestinationNode: "netperf-w2", Index: "10", Label:"iperf UDP. Same VM using Virtual IP", Type: iperfUdpTest, ClusterIP: true, MSS: mssMax},
		//{SourceNode: "netperf-w1", DestinationNode: "netperf-w3", Index: "11", Label:"iperf UDP. Remote VM using Pod IP", Type: iperfUdpTest, ClusterIP: false, MSS: mssMax},
		//{SourceNode: "netperf-w3", DestinationNode: "netperf-w2", Index: "12", Label:"iperf UDP. Remote VM using Virtual IP", Type: iperfUdpTest, ClusterIP: true, MSS: mssMax},
		{SourceNode: "netperf-w1", DestinationNode: "netperf-w4", Index: "13", Label:"iperf UDP. Same VM, primary node IP using NodePort", Type: iperfUdpTest, ClusterIP: false, UsePrimaryNodeIP: true, MSS: mssMax},
		{SourceNode: "netperf-w1", DestinationNode: "netperf-w4", Index: "14", Label:"iperf UDP. Same VM, secondary node IP using NodePort", Type: iperfUdpTest, ClusterIP: false, UsePrimaryNodeIP: false, MSS: mssMax},
		//{SourceNode: "netperf-w3", DestinationNode: "netperf-w4", Index: "15", Label:"iperf UDP. Remote VM,  primary node IP using NodePort", Type: iperfUdpTest, ClusterIP: false, UsePrimaryNodeIP: true, MSS: mssMax},
		{SourceNode: "netperf-w1", DestinationNode: "netperf-w2", Index: "16", Label:"netperf. Same VM using Pod IP", Type: netperfTest, ClusterIP: false},
		{SourceNode: "netperf-w1", DestinationNode: "netperf-w2", Index: "17", Label:"netperf. Same VM using Virtual IP", Type: netperfTest, ClusterIP: true},
		//{SourceNode: "netperf-w1", DestinationNode: "netperf-w3", Index: "18", Label:"netperf. Remote VM using Pod IP", Type: netperfTest, ClusterIP: false},
		//{SourceNode: "netperf-w3", DestinationNode: "netperf-w2", Index: "19", Label:"netperf. Remote VM using Virtual IP", Type: netperfTest, ClusterIP: true},
		{SourceNode: "netperf-w1", DestinationNode: "netperf-w2", Index: "20", Label:"fortio HTTP. Same VM using Pod IP", Type: fortioTest, ClusterIP: false, MSS: mssMax},
		{SourceNode: "netperf-w1", DestinationNode: "netperf-w2", Index: "21", Label:"fortio HTTP. Same VM using Virtual IP", Type: fortioTest, ClusterIP: true, MSS: mssMax},
		//{SourceNode: "netperf-w1", DestinationNode: "netperf-w3", Index: "22", Label:"fortio HTTP. Remote VM using Pod IP", Type: fortioTest, ClusterIP: false, MSS: mssMax},
		//{SourceNode: "netperf-w3", DestinationNode: "netperf-w2", Index: "23", Label:"fortio HTTP. Remote VM using Virtual IP", Type: fortioTest, ClusterIP: true, MSS: mssMax},
		{SourceNode: "netperf-w2", DestinationNode: "netperf-w2", Index: "24", Label:"fortio HTTP. Hairpin Pod to own Virtual IP", Type: fortioTest, ClusterIP: true, MSS: mssMax},
		{SourceNode: "netperf-w1", DestinationNode: "netperf-w4", Index: "25", Label:"fortio HTTP. Same VM, primary node IP using NodePort", Type: fortioTest, ClusterIP: false, UsePrimaryNodeIP: true,  MSS: mssMin},
		{SourceNode: "netperf-w1", DestinationNode: "netperf-w4", Index: "26", Label:"fortio HTTP. Same VM, secondary node IP using NodePort", Type: fortioTest, ClusterIP: false, UsePrimaryNodeIP: false, MSS: mssMin},
		//{SourceNode: "netperf-w3", DestinationNode: "netperf-w4", Index: "27", Label:"fortio HTTP. Remote VM, primary node IP using NodePort", Type: fortioTest, ClusterIP: false, UsePrimaryNodeIP: true, MSS: mssMin},
		{SourceNode: "netperf-w1", DestinationNode: "netperf-w2", Index: "28", Label:"ping. Same VM using Pod IP", Type: pingTest, ClusterIP: false, MSS: mssMax},
		//{SourceNode: "netperf-w1", DestinationNode: "netperf-w3", Index: "29", Label:"ping. Remote VM using Pod IP", Type: pingTest, ClusterIP: false, MSS: mssMax},
		{SourceNode: "netperf-w1", DestinationNode: "netperf-w4", Index: "31", Label:"ping. Same VM, primary node IP using NodePort", Type: pingTest, ClusterIP: false, UsePrimaryNodeIP: true,  MSS: mssMin},
		{SourceNode: "netperf-w1", DestinationNode: "netperf-w4", Index: "32", Label:"ping. Same VM, secondary node IP using NodePort", Type: pingTest, ClusterIP: false, UsePrimaryNodeIP: false, MSS: mssMin},
		//{SourceNode: "netperf-w3", DestinationNode: "netperf-w4", Index: "33", Label:"ping. Remote VM, primary node IP using NodePort", Type: pingTest, ClusterIP: false, UsePrimaryNodeIP: true, MSS: mssMin},
	}

	currentJobIndex = 0

	// Regexes to parse the Mbits/sec out of iperf TCP, UDP and netperf output
	iperfTCPOutputRegexp = regexp.MustCompile("SUM.*\\s+(\\d+)\\sMbits/sec\\s+receiver")
	iperfUDPOutputRegexp = regexp.MustCompile("\\s+(\\S+)\\sMbits/sec\\s+\\S+\\s+ms\\s+")
	netperfOutputRegexp = regexp.MustCompile("\\s+\\d+\\s+\\d+\\s+\\d+\\s+\\S+\\s+(\\S+)\\s+")
	iperfCPUOutputRegexp = regexp.MustCompile(`local/sender\s(\d+\.\d+)%\s\((\d+\.\d+)%\w/(\d+\.\d+)%\w\),\sremote/receiver\s(\d+\.\d+)%\s\((\d+\.\d+)%\w/(\d+\.\d+)%\w\)`)
	fortioOutputRegexp   = regexp.MustCompile("(?s)Aggregated.*avg\\s(\\S+)\\s\\S+\\s(\\S+).*50%\\s(\\S+).*75%\\s(\\S+).*99%\\s(\\S+).*99.9%\\s(\\S+)")
	pingOutputRegexp = regexp.MustCompile("(?s)(\\S+)%.*= (\\S+)\\/(\\S+)\\/(\\S+)\\/(\\S+)")

	dataPoints = make(map[string][]point)
	dataPointLabels = make(map[string]string)
}

func initializeOutputFiles() {
	fd, err := os.OpenFile(outputCaptureFile, os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		fmt.Println("Failed to open output capture file", err)
		os.Exit(2)
	}
	fd.Close()
}

func main() {
	initializeOutputFiles()
	flag.Parse()
	if !validateParams() {
		fmt.Println("Failed to parse cmdline args - fatal error - bailing out")
		os.Exit(1)

	}
	grabEnv()
	fmt.Println("Running as", mode, "...")
	if mode == orchestratorMode {
		orchestrate()
	} else {
		startWork()
	}
	fmt.Println("Terminating npd")
}

func grabEnv() {
	worker = os.Getenv("worker")
	kubenode = os.Getenv("kubenode")
	podname = os.Getenv("HOSTNAME")
	primaryNodeIP = os.Getenv("primaryNodeIP")
	secondaryNodeIP = os.Getenv("secondaryNodeIP")
	nodePort = os.Getenv("nodePort")
}

func validateParams() (rv bool) {
	rv = true
	if mode != workerMode && mode != orchestratorMode {
		fmt.Println("Invalid mode", mode)
		return false
	}

	if len(port) == 0 {
		fmt.Println("Invalid port", port)
		return false
	}

	if (len(host)) == 0 {
		if mode == orchestratorMode {
			host = os.Getenv("NETPERF_ORCH_SERVICE_HOST")
		} else {
			host = os.Getenv("NETPERF_ORCH_SERVICE_HOST")
		}
	}
	return
}

func allWorkersIdle() bool {
	for _, v := range workerStateMap {
		if !v.idle {
			return false
		}
	}
	return true
}

func getWorkerPodIP(worker string) string {
	return workerStateMap[worker].IP
}

func allocateWorkToClient(workerS *workerState, reply *WorkItem) {
	if !allWorkersIdle() {
		reply.IsIdle = true
		return
	}

	// System is all idle - pick up next work item to allocate to client
	for n, v := range testcases {
		if v.Finished {
			continue
		}
		if v.SourceNode != workerS.worker {
			reply.IsIdle = true
			return
		}
		if _, ok := workerStateMap[v.DestinationNode]; !ok {
			reply.IsIdle = true
			return
		}

		if mssStepSize == 0 {
			v.MSS = mssMax
			v.Finished = true
		}

		fmt.Printf("Requesting jobrun '%s' %s from %s to %s for MSS %d\n", v.Index, v.Label, v.SourceNode, v.DestinationNode, v.MSS)
		reply.ClientItem.Type = v.Type
		reply.IsClientItem = true
		workerS.idle = false
		currentJobIndex = n

		if !v.ClusterIP {
			//netperf-w4 pod is for NodePort measurements
			if v.DestinationNode == "netperf-w4" {
				reply.ClientItem.Port = workerS.nodePort
				if v.UsePrimaryNodeIP {
					reply.ClientItem.Host = workerS.primaryNodeIP
				} else {
					reply.ClientItem.Host = workerS.secondaryNodeIP
				}
			} else {
				reply.ClientItem.Host = getWorkerPodIP(v.DestinationNode)
				reply.ClientItem.Port = "5201"
			}
		} else {
			reply.ClientItem.Host = os.Getenv("NETPERF_W2_SERVICE_HOST")
			reply.ClientItem.Port = "5201"
		}

		switch {
		case v.Type == iperfTcpTest || v.Type == iperfUdpTest:
			reply.ClientItem.MSS = v.MSS

			v.MSS = v.MSS + mssStepSize
			if v.MSS > mssMax {
				v.Finished = true
			}
			return

		case v.Type == netperfTest:
			reply.ClientItem.Port = "12865"
			return

		case v.Type == fortioTest:
			reply.ClientItem.Port = "8080"
			return
		case v.Type == pingTest:
			return
		}
	}

	for _, v := range testcases {
		if !v.Finished {
			return
		}
	}

	if !datapointsFlushed {
		fmt.Println("ALL TESTCASES AND MSS RANGES COMPLETE - GENERATING CSV OUTPUT")
		flushDataPointsToCsv()
		datapointsFlushed = true
	}

	reply.IsIdle = true
}

// RegisterClient registers a single and assign a work item to it
func (t *NetPerfRpc) RegisterClient(data *ClientRegistrationData, reply *WorkItem) error {
	globalLock.Lock()
	defer globalLock.Unlock()

	state, ok := workerStateMap[data.Worker]

	if !ok {
		// For new clients, trigger an iperf server start immediately
		state = &workerState{sentServerItem: true, idle: true, IP: data.IP, worker: data.Worker, primaryNodeIP: data.PrimaryNodeIP, secondaryNodeIP: data.SecondaryNodeIP, nodePort: data.NodePort}
		workerStateMap[data.Worker] = state
		reply.IsServerItem = true
		reply.ServerItem.ListenPort = "5201"
		reply.ServerItem.Timeout = 3600
		return nil
	}

	// Worker defaults to idle unless the allocateWork routine below assigns an item
	state.idle = true

	// Give the worker a new work item or let it idle loop another 5 seconds
	allocateWorkToClient(state, reply)
	return nil
}

func writeOutputFile(filename, data string) {
	fd, err := os.OpenFile(filename, os.O_APPEND|os.O_WRONLY, 0666)
	if err != nil {
		fmt.Println("Failed to append to existing file", filename, err)
		return
	}
	defer fd.Close()

	if _, err = fd.WriteString(data); err != nil {
		fmt.Println("Failed to append to existing file", filename, err)
	}
}

func registerDataPoint(_type int, index string, label string, key string, value string) {
		if sl, ok := dataPoints[index]; !ok {
			dataPoints[index] = []point{{_type: _type, key: key, value: value}}
			dataPointKeys = append(dataPointKeys, index)
			dataPointLabels[index] = label
		} else {
			dataPoints[index] = append(sl, point{_type: _type,key: key, value: value})
		}
}

func flushDataPointsToCsv() {
	var buffer string
	var iperfTcpHeader, netperfHeader, fortioHeader, pingHeader bool = false, false, false, false

	for _, index := range dataPointKeys {
		for _, dp := range dataPoints[index] {
			switch dp._type{
				case iperfTcpTest:
					if !iperfTcpHeader {
						//Header for iperf tcp test cases
						buffer = fmt.Sprintf("%-60s,", "0")
						for _, p := range dataPoints[index] {
							buffer = buffer + fmt.Sprintf("%-15s,", p.key)
						}
						fmt.Println(buffer)
						buffer = ""
						iperfTcpHeader = true
					}

					// Data
					buffer = buffer + fmt.Sprintf("%-15s,", dp.value)

				case iperfUdpTest, netperfTest:
					if !netperfHeader {
						//Header for iperf udp and netperf test cases
						buffer = fmt.Sprintf("%-60s, Bandwidth", "0")
						fmt.Println(buffer)
						buffer = ""
						netperfHeader = true
					}
					
					// Data
					buffer = buffer + fmt.Sprintf("%-15s,", dp.value)

				case fortioTest:
					if !fortioHeader {
						//Header for fortio test cases
						buffer = fmt.Sprintf("%-60s,", "0")
						for _, p := range dataPoints[index] {
							buffer = buffer + fmt.Sprintf("%-15s,", p.key)
						}
						fmt.Println(buffer)
						buffer = ""
						fortioHeader = true
					}

					// Data
					buffer = buffer + fmt.Sprintf("%-15s,", dp.value)

				case pingTest:
					if !pingHeader {
						//Header for ping test cases
						buffer = fmt.Sprintf("%-60s,", "0")
						for _, p := range dataPoints[index] {
							buffer = buffer + fmt.Sprintf("%-15s,", p.key)
						}
						fmt.Println(buffer)
						buffer = ""
						pingHeader = true
					}

					// Data
					buffer = buffer + fmt.Sprintf("%-15s,", dp.value)
			}
		}
		fmt.Printf("%s,%-60s,", index, dataPointLabels[index])
		fmt.Println(buffer)
		buffer = ""	
	}
	fmt.Println("END CSV DATA")
}

func parseIperfTcpBandwidth(output string) string {
	// Parses the output of iperf3 and grabs the group Mbits/sec from the output
	match := iperfTCPOutputRegexp.FindStringSubmatch(output)
	if match != nil && len(match) > 1 {
		return match[1]
	}
	return "0"
}

func parseIperfUdpBandwidth(output string) string {
	// Parses the output of iperf3 (UDP mode) and grabs the Mbits/sec from the output
	match := iperfUDPOutputRegexp.FindStringSubmatch(output)
	if match != nil && len(match) > 1 {
		return match[1]
	}
	return "0"
}

func parseIperfCpuUsage(output string) (string, string) {
	// Parses the output of iperf and grabs the CPU usage on sender and receiver side from the output
	match := iperfCPUOutputRegexp.FindStringSubmatch(output)
	if match != nil && len(match) > 1 {
		return match[1], match[4]
	}
	return "0", "0"
}

func parseNetperfBandwidth(output string) string {
	// Parses the output of netperf and grabs the Bbits/sec from the output
	match := netperfOutputRegexp.FindStringSubmatch(output)
	if match != nil && len(match) > 1 {
		return match[1]
	}
	return "0"
}


func parseFortioResult(output string) []string {
	match := fortioOutputRegexp.FindStringSubmatch(output)
	if match != nil  && len(match) > 6 {
			return match
	}
	return []string{"0","0","0","0","0","0","0"}
}

func parsePingResult(output string) []string {
	match := pingOutputRegexp.FindStringSubmatch(output)
	if match != nil  && len(match) > 5 {
			return match
	}
	return []string{"0","0","0","0","0","0"}
}

// ReceiveOutput processes a data received from a single client
func (t *NetPerfRpc) ReceiveOutput(data *WorkerOutput, reply *int) error {
	globalLock.Lock()
	defer globalLock.Unlock()

	testcase := testcases[currentJobIndex]

	var outputLog string
	var bw string
	var cpuSender string
	var cpuReceiver string
	var res []string

	switch data.Type {
	case iperfTcpTest:
		mss := testcases[currentJobIndex].MSS - mssStepSize
		outputLog = outputLog + fmt.Sprintln("Received TCP output from worker", data.Worker, "for test", testcase.Label,
			"from", testcase.SourceNode, "to", testcase.DestinationNode, "MSS:", mss) + data.Output
		writeOutputFile(outputCaptureFile, outputLog)
		bw = parseIperfTcpBandwidth(data.Output)
		cpuSender, cpuReceiver = parseIperfCpuUsage(data.Output)
		registerDataPoint(data.Type, testcase.Index, testcase.Label, fmt.Sprintf("%d",mss), bw)

	case iperfUdpTest:
		mss := testcases[currentJobIndex].MSS - mssStepSize
		outputLog = outputLog + fmt.Sprintln("Received UDP output from worker", data.Worker, "for test", testcase.Label,
			"from", testcase.SourceNode, "to", testcase.DestinationNode, "MSS:", mss) + data.Output
		writeOutputFile(outputCaptureFile, outputLog)
		bw = parseIperfUdpBandwidth(data.Output)
		registerDataPoint(data.Type, testcase.Index, testcase.Label, fmt.Sprintf("%d",mss), bw)

	case netperfTest:
		outputLog = outputLog + fmt.Sprintln("Received netperf output from worker", data.Worker, "for test", testcase.Label,
			"from", testcase.SourceNode, "to", testcase.DestinationNode) + data.Output
		writeOutputFile(outputCaptureFile, outputLog)
		bw = parseNetperfBandwidth(data.Output)
		registerDataPoint(data.Type, testcase.Index, testcase.Label, "0", bw)
		testcases[currentJobIndex].Finished = true
		
	case fortioTest:
		outputLog = outputLog + fmt.Sprintln("Received fortio output from worker", data.Worker, "for test", testcase.Label,
				"from", testcase.SourceNode, "to", testcase.DestinationNode) + data.Output
		writeOutputFile(outputCaptureFile, outputLog)
		res = parseFortioResult(data.Output)
		registerDataPoint(data.Type, testcase.Index, testcase.Label, "avg", res[1])
		registerDataPoint(data.Type, testcase.Index, testcase.Label, "dev", res[2])
		registerDataPoint(data.Type, testcase.Index, testcase.Label, "50%", res[3])
		registerDataPoint(data.Type, testcase.Index, testcase.Label, "75%", res[4])
		registerDataPoint(data.Type, testcase.Index, testcase.Label, "99%", res[5])
		registerDataPoint(data.Type, testcase.Index, testcase.Label, "99,9%", res[6])
		testcases[currentJobIndex].Finished = true
	case pingTest:
		outputLog = outputLog + fmt.Sprintln("Received ping output from worker", data.Worker, "for test", testcase.Label,
				"from", testcase.SourceNode, "to", testcase.DestinationNode) + data.Output
		writeOutputFile(outputCaptureFile, outputLog)
		res = parsePingResult(data.Output)
		registerDataPoint(data.Type, testcase.Index, testcase.Label, "loss", res[1])
		registerDataPoint(data.Type, testcase.Index, testcase.Label, "min", res[2])
		registerDataPoint(data.Type, testcase.Index, testcase.Label, "avg", res[3])
		registerDataPoint(data.Type, testcase.Index, testcase.Label, "max", res[4])
		registerDataPoint(data.Type, testcase.Index, testcase.Label, "mdev", res[5])
		testcases[currentJobIndex].Finished = true		
	}

	switch data.Type {
	case iperfTcpTest:
		fmt.Println("Jobdone from worker", data.Worker, "Bandwidth was", bw, "Mbits/sec. CPU usage sender was", cpuSender, "%. CPU usage receiver was", cpuReceiver, "%.")
	case fortioTest:
		fmt.Println("Jobdone from worker", data.Worker, "avg", res[1] , "dev", res[2], "50%", res[3], "75%", res[4], "99%", res[5], "99,9%", res[6])
	case pingTest:
		fmt.Println("Jobdone from worker", data.Worker, "loss", res[1] , "ms min", res[2], "ms avg", res[3], "ms max", res[4], "ms mdev", res[5], "ms")
	default:
		fmt.Println("Jobdone from worker", data.Worker, "Bandwidth was", bw, "Mbits/sec")
	}

	return nil
}

func serveRPCRequests(port string) {
	baseObject := new(NetPerfRpc)
	rpc.Register(baseObject)
	rpc.HandleHTTP()
	listener, e := net.Listen("tcp", ":"+port)
	if e != nil {
		log.Fatal("listen error:", e)
	}
	http.Serve(listener, nil)
}

// Blocking RPC server start - only runs on the orchestrator
func orchestrate() {
	serveRPCRequests(rpcServicePort)
}

// Walk the list of interfaces and find the first interface that has a valid IP
// Inside a container, there should be only one IP-enabled interface
func getMyIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return localhostIPv4Address
	}

	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback == 0 {
			addrs, _ := iface.Addrs()
			for _, addr := range addrs {
				var ip net.IP
				switch v := addr.(type) {
				case *net.IPNet:
					ip = v.IP
				case *net.IPAddr:
					ip = v.IP
				}
				return ip.String()
			}
		}
	}
	return "127.0.0.1"
}

func handleClientWorkItem(client *rpc.Client, workItem *WorkItem) {
	fmt.Println("Orchestrator requests worker run item Type:", workItem.ClientItem.Type)
	switch {
	case workItem.ClientItem.Type == iperfTcpTest || workItem.ClientItem.Type == iperfUdpTest:
		outputString := iperfClient(workItem.ClientItem.Host, workItem.ClientItem.Port, workItem.ClientItem.MSS, workItem.ClientItem.Type)
		var reply int
		client.Call("NetPerfRpc.ReceiveOutput", WorkerOutput{Output: outputString, Worker: worker, Type: workItem.ClientItem.Type}, &reply)
	case workItem.ClientItem.Type == netperfTest:
		outputString := netperfClient(workItem.ClientItem.Host, workItem.ClientItem.Port, workItem.ClientItem.Type)
		var reply int
		client.Call("NetPerfRpc.ReceiveOutput", WorkerOutput{Output: outputString, Worker: worker, Type: workItem.ClientItem.Type}, &reply)
	case workItem.ClientItem.Type == fortioTest:
		outputString := fortioClient(workItem.ClientItem.Host, workItem.ClientItem.Port, workItem.ClientItem.Type)
		var reply int
		client.Call("NetPerfRpc.ReceiveOutput", WorkerOutput{Output: outputString, Worker: worker, Type: workItem.ClientItem.Type}, &reply)
	case workItem.ClientItem.Type == pingTest:
		outputString := pingClient(workItem.ClientItem.Host, workItem.ClientItem.Port, workItem.ClientItem.Type)
		var reply int
		client.Call("NetPerfRpc.ReceiveOutput", WorkerOutput{Output: outputString, Worker: worker, Type: workItem.ClientItem.Type}, &reply)
	}
	// Client COOLDOWN period before asking for next work item to replenish burst allowance policers etc
	time.Sleep(10 * time.Second)
}

// startWork : Entry point to the worker infinite loop
func startWork() {
	for true {
		var timeout time.Duration
		var client *rpc.Client
		var err error

		timeout = 5
		for true {
			fmt.Println("Attempting to connect to orchestrator at", host)
			client, err = rpc.DialHTTP("tcp", host+":"+port)
			if err == nil {
				break
			}
			fmt.Println("RPC connection to ", host, " failed:", err)
			time.Sleep(timeout * time.Second)
		}

		for true {
			clientData := ClientRegistrationData{Host: podname, KubeNode: kubenode, Worker: worker, IP: getMyIP(), PrimaryNodeIP: primaryNodeIP, SecondaryNodeIP: secondaryNodeIP, NodePort: nodePort}
			var workItem WorkItem
			if err := client.Call("NetPerfRpc.RegisterClient", clientData, &workItem); err != nil {
				// RPC server has probably gone away - attempt to reconnect
				fmt.Println("Error attempting RPC call", err)
				break
			}

			switch {
			case workItem.IsIdle == true:
				time.Sleep(5 * time.Second)
				continue

			case workItem.IsServerItem == true:
				fmt.Println("Orchestrator requests worker run iperf and netperf servers")
				go iperfServer()
				go netperfServer()
				go fortioServer()
				time.Sleep(1 * time.Second)

			case workItem.IsClientItem == true:
				handleClientWorkItem(client, &workItem)
			}
		}
	}
}

// Invoke and indefinitely run an iperf server
func iperfServer() {
	output, success := cmdExec(iperf3Path, []string{iperf3Path, "-s", host, "-J", "-i", "60"}, 15)
	if success {
		fmt.Println(output)
	}
}

// Invoke and indefinitely run netperf server
func netperfServer() {
	output, success := cmdExec(netperfServerPath, []string{netperfServerPath, "-D"}, 15)
	if success {
		fmt.Println(output)
	}
}

// Invoke and indefinitely run fortio server
func fortioServer() {
	output, success := cmdExec(fortioPath, []string{fortioPath, "server"}, 15)
	if success {
			fmt.Println(output)
	}
}

// Invoke and run an iperf client and return the output if successful.
func iperfClient(serverHost, serverPort string, mss int, workItemType int) (rv string) {
	switch {
	case workItemType == iperfTcpTest:
		output, success := cmdExec(iperf3Path, []string{iperf3Path, "-c", serverHost, "-p", serverPort, "-V", "-N", "-i", "30", "-t", "10", "-f", "m", "-w", "512M", "-Z", "-P", parallelStreams, "-M", strconv.Itoa(mss)}, 15)
		if success {
			rv = output
		}

	case workItemType == iperfUdpTest:
		output, success := cmdExec(iperf3Path, []string{iperf3Path, "-c", serverHost, "-p", serverPort, "-i", "30", "-t", "10", "-f", "m", "-b", "0", "-u"}, 15)
		if success {
			rv = output
		}
	}
	return
}

// Invoke and run a netperf client and return the output if successful.
func netperfClient(serverHost, serverPort string, workItemType int) (rv string) {
	output, success := cmdExec(netperfPath, []string{netperfPath, "-H", serverHost}, 15)
	if success {
		fmt.Println(output)
		rv = output
	} else {
		fmt.Println("Error running netperf client", output)
	}

	return
}


// Invoke and run a fortio client and return the output if successful.
func fortioClient(serverHost string, serverPort string, workItemType int) (rv string) {
	server := fmt.Sprintf("%s:8080", serverHost)
	fmt.Println(server)
	output, success := cmdExec(fortioPath, []string{fortioPath, "load", server}, 15)
	if success {
			fmt.Println(output)
			rv = output
	} else {
			fmt.Println("Error running fortio client", output)
	}

	return
}

// Invoke and run a ping client and return the output if successful.
func pingClient(serverHost string, serverPort string, workItemType int) (rv string) {
	output, success := cmdExec("/bin/ping", []string{"/bin/ping", "-c 10 -q", serverHost}, 15)
	if success {
			fmt.Println(output)
			rv = output
	} else {
			fmt.Println("Error running ping client", output)
	}

	return
}

func cmdExec(command string, args []string, timeout int32) (rv string, rc bool) {
	cmd := exec.Cmd{Path: command, Args: args}

	var stdoutput bytes.Buffer
	var stderror bytes.Buffer
	cmd.Stdout = &stdoutput
	cmd.Stderr = &stderror
	if err := cmd.Run(); err != nil {
		outputstr := stdoutput.String()
		errstr := stderror.String()
		fmt.Println("Failed to run", outputstr, "error:", errstr, err)
		return
	}

	rv = stdoutput.String()
	if command == fortioPath {
		rv = stderror.String()
	}
	fmt.Println(rv)
	rc = true
	return
}
