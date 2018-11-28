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
 launch.go

 Launch the netperf tests

 1. Launch the netperf-orch service
 2. Launch the worker pods
 3. Wait for the output csv data to show up in orchestrator pod logs
*/

package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	api "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	debugLog         = "output.txt"
	testNamespace    = "netperf"
	csvDataMarker    = "GENERATING CSV OUTPUT"
	csvEndDataMarker = "END CSV DATA"
	runUUID          = "latest"
	orchestratorPort = 5202
	iperf3Port       = 5201
	netperfPort      = 12865
	nodePort         = 30000
)

var (
	iterations         int
	hostnetworking     bool
	tag                string
	kubeConfig         string
	netperfImage       string
	cleanupOnly        bool
	backgroundPods     int
	backgroundPodImage string
	extraServices      int
	extraNetperfPods   int

	everythingSelector metav1.ListOptions = metav1.ListOptions{}

	primaryNode     api.Node
	secondaryNode   api.Node
	primaryNodeIP   string
	secondaryNodeIP string
)

func init() {
	flag.BoolVar(&hostnetworking, "hostnetworking", false,
		"(boolean) Enable Host Networking Mode for PODs")
	flag.IntVar(&iterations, "iterations", 1,
		"Number of iterations to run")
	flag.StringVar(&tag, "tag", runUUID, "CSV file suffix")
	flag.StringVar(&netperfImage, "image", "sirot/netperf-latest", "Docker image used to run the network tests")
	flag.StringVar(&kubeConfig, "kubeConfig", "",
		"Location of the kube configuration file ($HOME/.kube/config")
	flag.BoolVar(&cleanupOnly, "cleanup", false,
		"(boolean) Run the cleanup resources phase only (use this flag to clean up orphaned resources from a test run)")
	flag.IntVar(&backgroundPods, "backgroundPods", 0,
		"Number of pods to run in the background")
	flag.StringVar(&backgroundPodImage, "backgroundPodImage", "k8s.gcr.io/pause",
		"Docker image used to run the background pods")
	flag.IntVar(&extraServices, "extraServices", 0,
		"Number of services to run beside netperf-w2")
	flag.IntVar(&extraNetperfPods, "extraNetperfPods", 0,
		"Number of extra netperf-w2 pods")
}

func setupClient() *kubernetes.Clientset {
	config, err := clientcmd.BuildConfigFromFlags("", kubeConfig)
	if err != nil {
		panic(err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err)
	}

	return clientset
}

// getMinions : Only return schedulable/worker nodes
func getMinionNodes(c *kubernetes.Clientset) *api.NodeList {
	nodes, err := c.Core().Nodes().List(
		metav1.ListOptions{
			FieldSelector: "spec.unschedulable=false",
		})
	if err != nil {
		fmt.Println("Failed to fetch nodes", err)
		return nil
	}
	return nodes
}

//get the Internal IP address of a node
func getNodeIP(n *api.Node) string {
	nodeIP := ""
	for _, addr := range n.Status.Addresses {
		if addr.Type == api.NodeInternalIP {
			nodeIP = addr.Address
		}
	}
	return nodeIP
}

func cleanup(c *kubernetes.Clientset) {
	// Cleanup existing rcs, pods and services in our namespace
	rcs, err := c.Core().ReplicationControllers(testNamespace).List(everythingSelector)
	if err != nil {
		fmt.Println("Failed to get replication controllers", err)
		return
	}
	for _, rc := range rcs.Items {
		fmt.Println("Deleting rc", rc.GetName())
		if err := c.Core().ReplicationControllers(testNamespace).Delete(
			rc.GetName(), &metav1.DeleteOptions{}); err != nil {
			fmt.Println("Failed to delete rc", rc.GetName(), err)
		}
	}
	pods, err := c.Core().Pods(testNamespace).List(everythingSelector)
	if err != nil {
		fmt.Println("Failed to get pods", err)
		return
	}
	for _, pod := range pods.Items {
		fmt.Println("Deleting pod", pod.GetName())
		if err := c.Core().Pods(testNamespace).Delete(pod.GetName(), &metav1.DeleteOptions{GracePeriodSeconds: new(int64)}); err != nil {
			fmt.Println("Failed to delete pod", pod.GetName(), err)
		}
	}
	svcs, err := c.Core().Services(testNamespace).List(everythingSelector)
	if err != nil {
		fmt.Println("Failed to get services", err)
		return
	}
	for _, svc := range svcs.Items {
		fmt.Println("Deleting svc", svc.GetName())
		c.Core().Services(testNamespace).Delete(
			svc.GetName(), &metav1.DeleteOptions{})
	}
}

// createServices: Long-winded function to programmatically create our three services
func createServices(c *kubernetes.Clientset) bool {
	// Create our namespace if not present
	if _, err := c.Core().Namespaces().Get(testNamespace, metav1.GetOptions{}); err != nil {
		c.Core().Namespaces().Create(&api.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace}})
	}

	// Create the orchestrator service that points to the coordinator pod
	orchLabels := map[string]string{"app": "netperf-orch"}
	orchService := &api.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "netperf-orch",
		},
		Spec: api.ServiceSpec{
			Selector: orchLabels,
			Ports: []api.ServicePort{{
				Name:       "netperf-orch",
				Protocol:   api.ProtocolTCP,
				Port:       orchestratorPort,
				TargetPort: intstr.FromInt(orchestratorPort),
			}},
			Type: api.ServiceTypeClusterIP,
		},
	}
	if _, err := c.Core().Services(testNamespace).Create(orchService); err != nil {
		fmt.Println("Failed to create orchestrator service", err)
		return false
	}
	fmt.Println("Created orchestrator service")

	// Create the netperf-w2 service that points a clusterIP at the worker 2 pod
	for i := 0; i <= extraServices; i++ {
		netperfW2Labels := map[string]string{"app": "netperf-w2"}
		name := "netperf-w2"
		if i>0 {
			name = fmt.Sprintf("netperf-w2-%d", i)
		}
		netperfW2Service := &api.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
			},
			Spec: api.ServiceSpec{
				Selector: netperfW2Labels,
				Ports: []api.ServicePort{
					{
						Name:       "netperf-w2",
						Protocol:   api.ProtocolTCP,
						Port:       iperf3Port,
						TargetPort: intstr.FromInt(iperf3Port),
					},
					{
						Name:       "netperf-w2-udp",
						Protocol:   api.ProtocolUDP,
						Port:       iperf3Port,
						TargetPort: intstr.FromInt(iperf3Port),
					},
					{
						Name:       "netperf-w2-netperf",
						Protocol:   api.ProtocolTCP,
						Port:       netperfPort,
						TargetPort: intstr.FromInt(netperfPort),
					},
				},
				Type: api.ServiceTypeClusterIP,
			},
		}
		if _, err := c.Core().Services(testNamespace).Create(netperfW2Service); err != nil {
			fmt.Println("Failed to create service", name, err)
			return false
		}
		fmt.Println("Created service", name)
	}

	// Create the netperf-w4 service that points a clusterIP at the worker 4 pod and exposes a NodePort
	netperfW4Labels := map[string]string{"app": "netperf-w4"}
	netperfW4Service := &api.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "netperf-w4",
		},
		Spec: api.ServiceSpec{
			Selector: netperfW4Labels,
			Ports: []api.ServicePort{
				{
					Name:       "netperf-w4",
					Protocol:   api.ProtocolTCP,
					Port:       iperf3Port,
					TargetPort: intstr.FromInt(iperf3Port),
					NodePort:   nodePort,
				},
				{
					Name:       "netperf-w4-udp",
					Protocol:   api.ProtocolUDP,
					Port:       iperf3Port,
					TargetPort: intstr.FromInt(iperf3Port),
					NodePort:    nodePort,
				},
				{
					Name:       "netperf-w4-netperf",
					Protocol:   api.ProtocolTCP,
					Port:       netperfPort,
					TargetPort: intstr.FromInt(netperfPort),
				},
			},
			Type: api.ServiceTypeNodePort,
		},
	}
	if _, err := c.Core().Services(testNamespace).Create(netperfW4Service); err != nil {
		fmt.Println("Failed to create netperf-w4 service", err)
		return false
	}
	fmt.Println("Created netperf-w4 service")
	return true
}

// createRCs - Create replication controllers for all workers and the orchestrator
func createRCs(c *kubernetes.Clientset) bool {
	// Create the orchestrator RC
	name := "netperf-orch"
	fmt.Println("Creating replication controller", name)
	replicas := int32(1)

	_, err := c.Core().ReplicationControllers(testNamespace).Create(&api.ReplicationController{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: api.ReplicationControllerSpec{
			Replicas: &replicas,
			Selector: map[string]string{"app": name},
			Template: &api.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": name},
				},
				Spec: api.PodSpec{
					Containers: []api.Container{
						{
							Name:            name,
							Image:           netperfImage,
							Ports:           []api.ContainerPort{{ContainerPort: orchestratorPort}},
							Args:            []string{"--mode=orchestrator"},
							ImagePullPolicy: "Always",
						},
					},
					TerminationGracePeriodSeconds: new(int64),
				},
			},
		},
	})
	if err != nil {
		fmt.Println("Error creating orchestrator replication controller", err)
		return false
	}
	fmt.Println("Created orchestrator replication controller")
	//Create worker RCs
	for i := 1; i <= 4; i++ {
		// Bring up pods slowly
		time.Sleep(3 * time.Second)
		kubeNode := primaryNode.GetName()
		if i == 3 {
			kubeNode = secondaryNode.GetName()
		}
		name = fmt.Sprintf("netperf-w%d", i)
		fmt.Println("Creating replication controller", name)
		portSpec := []api.ContainerPort{}
		if i > 1 {
			// Worker W1 is a client-only pod - no ports are exposed
			portSpec = append(portSpec, api.ContainerPort{ContainerPort: iperf3Port, Protocol: api.ProtocolTCP})
		}

		workerEnv := []api.EnvVar{
			{Name: "worker", Value: name},
			{Name: "kubeNode", Value: kubeNode},
			{Name: "podname", Value: name},
			{Name: "primaryNodeIP", Value: primaryNodeIP},
			{Name: "secondaryNodeIP", Value: secondaryNodeIP},
			{Name: "nodePort", Value: fmt.Sprintf("%d", nodePort)},
		}

		replicas := int32(1)

		label := map[string]string{"app": name}
		selector := map[string]string{"app": name}

		if (i == 2 && extraNetperfPods > 0) {
			label = map[string]string{"app": name, "rcselector": "replicaforw2"}
			selector = map[string]string{"rcselector": "replicaforw2"}
		}

		_, err := c.Core().ReplicationControllers(testNamespace).Create(&api.ReplicationController{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: api.ReplicationControllerSpec{
				Replicas: &replicas,
				Selector: selector,
				Template: &api.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: label,
					},
					Spec: api.PodSpec{
						NodeName: kubeNode,
						Containers: []api.Container{
							{
								Name:            name,
								Image:           netperfImage,
								Ports:           portSpec,
								Args:            []string{"--mode=worker"},
								Env:             workerEnv,
								ImagePullPolicy: "Always",
							},
						},
						TerminationGracePeriodSeconds: new(int64),
					},
				},
			},
		})
		if err != nil {
			fmt.Println("Error creating orchestrator replication controller", name, ":", err)
			return false
		}
	}

	return true
}

// createPods - Create background pods for scalability tests
func createPods(c *kubernetes.Clientset) bool {
	name := "netperf-background"
	kubeNode := primaryNode.GetName()
	portSpec := []api.ContainerPort{}
	for i := 1; i <= backgroundPods; i++ {
		if i%2 == 0 {
			kubeNode = secondaryNode.GetName()
		} else {
			kubeNode = primaryNode.GetName()
		}
		name = fmt.Sprintf("netperf-background-%03d", i)
		_, err := c.Core().Pods(testNamespace).Create(&api.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Labels: map[string]string{"app": name},
			},
			Spec: api.PodSpec{
				NodeName: kubeNode,
				Containers: []api.Container{
					{
					Name:            name,
					Image:           backgroundPodImage,
					Ports:           portSpec,
					Args:            []string{"--mode=worker"},
					ImagePullPolicy: "Always",
					},
				},
				TerminationGracePeriodSeconds: new(int64),
			},
		})
		if err != nil {
			fmt.Printf("Error creating %s pod", name, ":", err)
			return false
		}
	}

	kubeNode = primaryNode.GetName()
	portSpec = append(portSpec, api.ContainerPort{ContainerPort: iperf3Port, Protocol: api.ProtocolTCP})
	for i := 1; i <= extraNetperfPods; i++ {
		name = fmt.Sprintf("netperf-w2-%03d", i)
		_, err := c.Core().Pods(testNamespace).Create(&api.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Labels: map[string]string{"app": "netperf-w2"},
			},
			Spec: api.PodSpec{
				NodeName: kubeNode,
				Containers: []api.Container{
					{
					Name:            name,
					Image:           netperfImage,
					Ports:           portSpec,
					Args:            []string{"--mode=worker"},
					ImagePullPolicy: "Always",
					},
				},
				TerminationGracePeriodSeconds: new(int64),
			},
		})
		if err != nil {
			fmt.Printf("Error creating %s pod", name, ":", err)
			return false
		}
	}
	return true
}

func getOrchestratorPodName(pods *api.PodList) string {
	for _, pod := range pods.Items {
		if strings.Contains(pod.GetName(), "netperf-orch-") {
			return pod.GetName()
		}
	}
	return ""
}

// Retrieve the logs for the pod/container and check if csv data has been generated
func getCsvResultsFromPod(c *kubernetes.Clientset, podName string) *string {
	body, err := c.Core().Pods(testNamespace).GetLogs(podName, &api.PodLogOptions{Timestamps: false}).DoRaw()
	if err != nil {
		fmt.Printf("Error (%s) reading logs from pod %s", err, podName)
		return nil
	}
	logData := string(body)
	index := strings.Index(logData, csvDataMarker)
	endIndex := strings.Index(logData, csvEndDataMarker)
	if index == -1 || endIndex == -1 {
		return nil
	}
	csvData := string(body[index+len(csvDataMarker)+1 : endIndex])
	return &csvData
}

// processCsvData : Process the CSV datafile and generate line and bar graphs
func processCsvData(csvData *string) bool {
	t := time.Now().UTC()
	outputFileDirectory := fmt.Sprintf("results_%s-%s", testNamespace, tag)
	outputFilePrefix := fmt.Sprintf("%s-%s_%s.", testNamespace, tag, t.Format("20060102150405"))
	fmt.Printf("Test concluded - CSV raw data written to %s/%scsv\n", outputFileDirectory, outputFilePrefix)
	if _, err := os.Stat(outputFileDirectory); os.IsNotExist(err) {
		os.Mkdir(outputFileDirectory, 0766)
	}
	fd, err := os.OpenFile(fmt.Sprintf("%s/%scsv", outputFileDirectory, outputFilePrefix), os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		fmt.Println("ERROR writing output CSV datafile", err)
		return false
	}
	fd.WriteString(*csvData)
	fd.Close()
	return true
}

func executeTests(c *kubernetes.Clientset) bool {
	for i := 0; i < iterations; i++ {
		cleanup(c)
		if !createServices(c) {
			fmt.Println("Failed to create services - aborting test")
			return false
		}
		time.Sleep(3 * time.Second)
		if !createRCs(c) {
			fmt.Println("Failed to create replication controllers - aborting test")
			return false
		}
		fmt.Println("Waiting for netperf pods to start up")
		if !createPods(c) {
			fmt.Println("Failed to create background pods - aborting test")
			return false
		}

		var orchestratorPodName string
		for len(orchestratorPodName) == 0 {
			fmt.Println("Waiting for orchestrator pod creation")
			time.Sleep(60 * time.Second)
			var pods *api.PodList
			var err error
			if pods, err = c.Core().Pods(testNamespace).List(everythingSelector); err != nil {
				fmt.Println("Failed to fetch pods - waiting for pod creation", err)
				continue
			}
			orchestratorPodName = getOrchestratorPodName(pods)
		}
		fmt.Println("Orchestrator Pod is", orchestratorPodName)

		// The pods orchestrate themselves, we just wait for the results file to show up in the orchestrator container
		for true {
			// Monitor the orchestrator pod for the CSV results file
			csvdata := getCsvResultsFromPod(c, orchestratorPodName)
			if csvdata == nil {
				fmt.Println("Scanned orchestrator pod filesystem - no results file found yet...waiting for orchestrator to write CSV file...")
				time.Sleep(60 * time.Second)
				continue
			}
			if processCsvData(csvdata) {
				break
			}
		}
		fmt.Printf("TEST RUN (Iteration %d) FINISHED - cleaning up services and pods\n", i)
	}
	return false
}

func main() {
	flag.Parse()
	fmt.Println("Network Performance Test")
	fmt.Println("Parameters :")
	fmt.Println("Iterations           : ", iterations)
	fmt.Println("Host Networking      : ", hostnetworking)
	fmt.Println("Docker image         : ", netperfImage)
	fmt.Println("Background pods      : ", backgroundPods)
	fmt.Println("Background pod image : ", backgroundPodImage)
	fmt.Println("Extra services       : ", extraServices)
	fmt.Println("Extra netperf pods   : ", extraNetperfPods)
	fmt.Println("------------------------------------------------------------")

	var c *kubernetes.Clientset
	if c = setupClient(); c == nil {
		fmt.Println("Failed to setup REST client to Kubernetes cluster")
		return
	}
	if cleanupOnly {
		cleanup(c)
		return
	}
	nodes := getMinionNodes(c)
	if nodes == nil {
		return
	}
	if len(nodes.Items) < 2 {
		fmt.Println("Insufficient number of nodes for test (need minimum 2 nodes)")
		return
	}
	primaryNode = nodes.Items[0]
	secondaryNode = nodes.Items[1]
	fmt.Printf("Selected primary,secondary nodes = (%s, %s)\n", primaryNode.GetName(), secondaryNode.GetName())
	primaryNodeIP = getNodeIP(&primaryNode)
	secondaryNodeIP = getNodeIP(&secondaryNode)
	if (primaryNodeIP == "" || secondaryNodeIP == "") {
		fmt.Println("Failed to get node IPs")
		return
	} else {
		fmt.Printf("Primary node IP = %s, secondary node IP = %s\n", primaryNodeIP, secondaryNodeIP)
	}
	executeTests(c)
	cleanup(c)
}
