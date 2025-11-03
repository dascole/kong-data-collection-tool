/*
Copyright © 2022 John Harris
Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:
The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.
THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
*/

package cmd

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kong/go-database-reconciler/pkg/konnect"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/kong/go-database-reconciler/pkg/dump"
	"github.com/kong/go-database-reconciler/pkg/file"
	"github.com/kong/go-database-reconciler/pkg/state"
	"github.com/kong/go-database-reconciler/pkg/utils"
	"github.com/kong/go-kong/kong"
	"github.com/pkg/errors"
	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/disk"
	"github.com/shirou/gopsutil/mem"
	"github.com/shirou/gopsutil/process"
	"github.com/shirou/gopsutil/v4/net"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/stretchr/objx"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kjson "k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
)

const (
	Docker           = "docker"
	Kubernetes       = "kubernetes"
	VM               = "vm"
	LineLimitDefault = int64(1000)
	vmMemoryLogFile  = "vm-memory-summary.json"
	vmCPULogFile     = "vm-cpu-summary.json"
	vmDiskLogFile    = "vm-disk-summary.json"
	vmProcessLogFile = "vm-process-summary.json"
	vmNetworkLogFile = "vm-network-summary.json"
	vmHostsFile      = "vm-hosts"
	vmResolvFile     = "vm-resolv.conf"
)

var (
	rType                      string
	konnectMode                bool
	controlPlaneName           string
	kongImages                 []string
	deckHeaders                []string
	targetPods                 []string
	kongConf                   string
	prefixDir                  string
	logsSinceDocker            string
	lineLimit                  int64
	logsSinceSeconds           int64
	clientTimeout              time.Duration
	rootConfig                 objx.Map
	kongAddr                   string
	dumpConfig                 dump.Config
	createWorkspaceConfigDumps bool
	disableKDDCollection       bool
	strToRedact                []string
)

// initLogging sets up logrus with standard configuration
func initLogging() {
	// Configure logrus with timestamp, level, and caller information
	log.SetFormatter(&log.TextFormatter{
		FullTimestamp:   true,
		TimestampFormat: "2006-01-02 15:04:05",
	})
	// Set output to stdout
	log.SetOutput(os.Stdout)
	// Set default log level
	log.SetLevel(log.InfoLevel)
}

type Summary struct {
	Version  string
	Portal   string
	Vitals   string
	DBMode   string
	Platform string
}

type MemoryInfo struct {
	PhysicalTotal     float64
	PhysicalAvailable float64
	SwapTotal         float64
	SwapFree          float64
}

type CPUInfo struct {
	CPU   int
	Cores int32
}

type DiskInfo struct {
	Total       uint64
	Free        uint64
	Used        uint64
	UsedPercent float64
}

type ProcessInfo struct {
	Name       string
	PID        int32
	CPUPercent string
	MemPercent uint64
	CmdLine    string
}

type NetworkInfo struct {
	Fd     uint32  `json:"fd"`
	Family uint32  `json:"family"`
	Type   uint32  `json:"type"`
	Laddr  string  `json:"localaddr"`
	Raddr  string  `json:"remoteaddr"`
	Status string  `json:"status"`
	Uids   []int32 `json:"uids"`
	Pid    int32   `json:"pid"`
}

type PortForwardAPodRequest struct {
	// RestConfig is the kubernetes config
	RestConfig *rest.Config
	// Pod is the selected pod for this port forwarding
	Pod corev1.Pod
	// LocalPort is the local port that will be selected to expose the PodPort
	LocalPort int
	// PodPort is the target port for the pod
	PodPort int
	// Steams configures where to write or read input from
	Streams genericclioptions.IOStreams
	// StopCh is the channel used to manage the port forward lifecycle
	StopCh <-chan struct{}
	// ReadyCh communicates when the tunnel is ready to receive traffic
	ReadyCh chan struct{}
}

type NamedCommand struct {
	Cmd  []string
	Name string
}

var collectCmd = &cobra.Command{
	Use:    "collect",
	Short:  "Collect Kong and Environment information",
	Long:   `Collect Kong and Environment information.`,
	PreRun: toggleDebug,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Initialize logging at the start
		initLogging()

		var filesToZip []string

		filesToCopy := []string{
			"/etc/resolv.conf",
			"/etc/hosts",
			"/etc/os-release",
		}

		commandsToRun := []NamedCommand{
			{Cmd: []string{"top", "-b", "-n", "1"}, Name: "top"},
			{Cmd: []string{"ls", "-lart", "/usr/local/share/lua/5.1/kong/templates"}, Name: "templates"},
			{Cmd: []string{"sh", "-c", "ulimit", "-n"}, Name: "ulimit"},
			{Cmd: []string{"uname", "-a"}, Name: "uname"},
			{Cmd: []string{"ps", "aux"}, Name: "ps"},
			{Cmd: []string{"df", "-h"}, Name: "df"},
			{Cmd: []string{"free", "-h"}, Name: "free"},
		}

		if rType == "" {
			rType = os.Getenv("KONG_RUNTIME")
		}

		if rType == "" {
			log.Info("No runtime detected, attempting to guess runtime...")
			runtime, err := guessRuntime()
			if err != nil {
				log.WithError(err).Error("Failed to guess runtime")
				return err
			}
			rType = runtime
		}

		switch rType {
		case "docker":
			log.Info("Using Docker runtime")
			dockerFilesToZip, err := runDocker(filesToCopy, commandsToRun)
			if err != nil {
				log.WithError(err).Error("Error with docker runtime collection")
			} else {
				filesToZip = append(filesToZip, dockerFilesToZip...)
			}

		case "kubernetes":
			log.Info("Using Kubernetes runtime")
			k8sFilesToZip, err := runKubernetes(filesToCopy, commandsToRun)
			if err != nil {
				log.WithError(err).Error("Error with Kubernetes runtime collection")
			} else {
				filesToZip = append(filesToZip, k8sFilesToZip...)
			}

		case "vm":
			log.Info("Using VM runtime")
			vmFilesToZip, err := runVM()
			if err != nil {
				log.WithError(err).Error("Error with VM runtime collection")
			} else {
				filesToZip = append(filesToZip, vmFilesToZip...)
			}
		default:
			log.WithField("runtime", rType).Error("Runtime not supported")
		}

		if os.Getenv("DISABLE_KDD") != "" {
			disableKDDCollection = (os.Getenv("DISABLE_KDD") == "true")
		}

		if disableKDDCollection && createWorkspaceConfigDumps {
			log.Warn("Cannot create workspaces dumps when KDD collection is disabled")
		}

		if !disableKDDCollection {
			log.Info("KDD collection is enabled")

			kddFilesToZip, err := getKDD()
			if err != nil {
				log.WithError(err).Error("Error with KDD collection")
			} else {
				filesToZip = append(filesToZip, kddFilesToZip...)
			}
		}

		log.Info("Writing tar.gz output")

		err := writeFiles(filesToZip)
		if err != nil {
			log.WithError(err).Error("Error writing tar.gz file")
		}

		return nil
	},
}

var (
	defaultKongImageList = []string{"kong-gateway", "kubernetes-ingress-controller"}
)

func init() {
	rootCmd.AddCommand(collectCmd)
	collectCmd.PersistentFlags().StringVarP(&controlPlaneName, "konnect-control-plane-name", "c", "", "Konnect Control Plane name.")
	collectCmd.PersistentFlags().StringVarP(&rType, "runtime", "r", "", "Runtime to extract logs from (kubernetes or docker). Runtime is auto detected if omitted.")
	collectCmd.PersistentFlags().BoolVarP(&konnectMode, "konnect-mode", "x", false, "Enables Konnect mode. This will use the Konnect API to collect data.")
	collectCmd.PersistentFlags().StringSliceVarP(&kongImages, "target-images", "i", defaultKongImageList, `Override default gateway images to scrape logs from. Default: "kong-gateway","kubernetes-ingress-controller"`)
	collectCmd.PersistentFlags().StringSliceVarP(&deckHeaders, "rbac-header", "H", nil, "RBAC header required to contact the admin-api.")
	collectCmd.PersistentFlags().StringVarP(&kongAddr, "kong-addr", "a", "http://localhost:8001", "The address to reach the admin-api of the Kong instance in question.")
	collectCmd.PersistentFlags().BoolVarP(&createWorkspaceConfigDumps, "dump-workspace-configs", "d", false, "Deck dump workspace configs to yaml files. Default: false. NOTE: Will not work if --disable-kdd=true")
	collectCmd.PersistentFlags().StringSliceVarP(&targetPods, "target-pods", "p", nil, "CSV list of pod names to target when extracting logs. Default is to scan all running pods for Kong images.")
	collectCmd.PersistentFlags().StringVar(&logsSinceDocker, "docker-since", "", "Return logs newer than a relative duration like 5s, 2m, or 3h. Used with docker runtime only. Will override --line-limit if set.")
	collectCmd.PersistentFlags().Int64Var(&logsSinceSeconds, "k8s-since-seconds", 0, "Return logs newer than the seconds past. Used with K8s runtime only. Will override --line-limit if set.")
	collectCmd.PersistentFlags().Int64Var(&lineLimit, "line-limit", LineLimitDefault, "Return logs with this amount of lines retrieved. Defaults to 1000 lines. Used with all runtimes as a default. --k8s-since-seconds and --docker-since will both override this setting.")
	collectCmd.PersistentFlags().StringVarP(&prefixDir, "prefix-dir", "k", "/usr/local/kong", "The path to your prefix directory for determining VM log locations. Default: /usr/local/kong")
	collectCmd.PersistentFlags().BoolVarP(&disableKDDCollection, "disable-kdd", "q", false, "Disable KDD config collection. Default: false.")
	collectCmd.PersistentFlags().StringSliceVarP(&strToRedact, "redact-logs", "R", nil, "CSV list of terms to redact during log extraction.")
}

func formatJSON(data []byte) ([]byte, error) {
	var out bytes.Buffer
	err := json.Indent(&out, data, "", "    ")
	if err == nil {
		return out.Bytes(), err
	}
	return data, nil
}

func guessRuntime() (string, error) {
	log.Info("Trying to guess runtime...")
	var errList []string
	ctx := context.Background()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		errList = append(errList, err.Error())
	}

	_, err = cli.ServerVersion(ctx)
	if err != nil {
		errList = append(errList, err.Error())
	}

	containers, err := cli.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		errList = append(errList, err.Error())
	}

	var kongContainers []types.Container

	for _, container := range containers {
		for _, i := range kongImages {
			if strings.Contains(container.Image, i) {
				kongContainers = append(kongContainers, container)
			}
		}
	}

	if len(kongContainers) > 0 {
		log.Info("Docker runtime detected")
		return Docker, nil
	}

	var kongK8sPods []string

	kubeClient, _, err := createClient()

	if err != nil {
		errList = append(errList, err.Error())
	} else {
		pl, err := kubeClient.CoreV1().Pods("").List(context.Background(), v1.ListOptions{})

		if err != nil {
			errList = append(errList, err.Error())
		} else {
			for _, p := range pl.Items {
				for _, c := range p.Spec.Containers {
					for _, i := range kongImages {
						if strings.Contains(c.Image, i) {
							kongK8sPods = append(kongK8sPods, p.Name)
						}
					}
				}
			}

			if len(kongK8sPods) > 0 {
				log.Info("Kubernetes runtime detected")
				return Kubernetes, nil
			}
		}
	}

	//If environment files exist, then VM install
	if _, err := os.Stat("/usr/local/kong/.kong_env"); err == nil {
		prefixDir = "/usr/local/kong"
		log.Info("VM runtime detected")
		return VM, nil
	} else {
		errList = append(errList, err.Error())

		//try /KONG_PREFIX
		if _, err := os.Stat("/KONG_PREFIX/.kong_env"); err == nil {
			prefixDir = "/KONG_PREFIX"
			log.Info("VM runtime detected with alternate prefix directory")
			return VM, nil
		} else {
			errList = append(errList, err.Error())
		}
	}

	return "", fmt.Errorf(strings.Join(errList, "\n"))
}

func runDocker(filesToCopy []string, commandsToRun []NamedCommand) ([]string, error) {
	ctx := context.Background()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.WithError(err).Error("Unable to create docker api client")
		return nil, err
	}

	containers, err := cli.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		log.WithError(err).Error("Unable to get container list from docker api")
		return nil, err
	}

	log.WithField("count", len(containers)).Info("Found containers running")

	// Pre-allocate with reasonable capacity
	kongContainers := make([]types.Container, 0, 10)

	for _, container := range containers {
		for _, i := range kongImages {
			if strings.Contains(container.Image, i) {
				kongContainers = append(kongContainers, container)
			}
		}
	}

	log.WithField("count", len(kongContainers)).Info("Found Kong containers")

	// Pre-allocate with estimated capacity (files per container * container count)
	filesToZip := make([]string, 0, len(kongContainers)*5)

	for _, c := range kongContainers {
		log.WithField("containerID", c.ID).Info("Inspecting container")

		copiedFiles, err := CopyFilesFromContainers(ctx, cli, c.ID, filesToCopy)
		if err != nil {
			log.WithFields(log.Fields{
				"containerID": c.ID,
				"error":       err,
			}).Error("Error copying files from container")
		}

		executedFiles, err := RunCommandsInContainer(ctx, cli, c.ID, commandsToRun)
		if err != nil {
			log.WithFields(log.Fields{
				"containerID": c.ID,
				"error":       err,
			}).Error("Error running commands in container")
		}

		log.WithField("count", len(copiedFiles)).Debug("Files copied from container")
		filesToZip = append(filesToZip, copiedFiles...)
		filesToZip = append(filesToZip, executedFiles...)

		_, b, err := cli.ContainerInspectWithRaw(ctx, c.ID, false)
		if err != nil {
			log.WithFields(log.Fields{
				"containerID": c.ID,
				"error":       err,
			}).Error("Unable to inspect container")
			continue
		}

		prettyJSON, err := formatJSON(b)
		if err != nil {
			log.WithError(err).Error("Unable to format JSON")
			continue
		}

		sanitizedImageName := strings.ReplaceAll(strings.ReplaceAll(c.Image, ":", "/"), "/", "-")
		sanitizedContainerName := strings.ReplaceAll(c.Names[0], "/", "")
		inspectFilename := fmt.Sprintf("%s-%s.json", sanitizedContainerName, sanitizedImageName)
		inspectFile, err := os.Create(inspectFilename)
		if err != nil {
			log.WithFields(log.Fields{
				"filename": inspectFilename,
				"error":    err,
			}).Error("Unable to create inspection file")
			continue
		}

		log.WithFields(log.Fields{
			"container": sanitizedContainerName,
			"filename":  inspectFilename,
		}).Info("Writing docker inspect data")

		_, err = io.Copy(inspectFile, bytes.NewReader(prettyJSON))
		if err != nil {
			log.WithError(err).Error("Unable to write inspect file")
			inspectFile.Close()
			continue
		}

		err = inspectFile.Close()
		if err != nil {
			log.WithError(err).Error("Unable to close inspect file")
			continue
		}

		filesToZip = append(filesToZip, inspectFilename)

		logsFilename := fmt.Sprintf("%s-%s.log", sanitizedContainerName, sanitizedImageName)
		logFile, err := os.Create(logsFilename)
		if err != nil {
			log.WithFields(log.Fields{
				"filename": logsFilename,
				"error":    err,
			}).Error("Unable to create container log file")
			continue
		}

		if os.Getenv("DOCKER_LOGS_SINCE") != "" {
			logsSinceDocker = os.Getenv("DOCKER_LOGS_SINCE")
		}

		options := container.LogsOptions{}

		if logsSinceDocker != "" {
			options = container.LogsOptions{ShowStdout: true, ShowStderr: true, Since: logsSinceDocker, Details: true}
			log.WithField("since", logsSinceDocker).Debug("Using time-based log retrieval")
		} else {
			strLineLimit := strconv.Itoa(int(lineLimit))
			options = container.LogsOptions{ShowStdout: true, ShowStderr: true, Tail: strLineLimit, Details: true}
			log.WithField("lineLimit", lineLimit).Debug("Using line-based log retrieval")
		}

		logs, err := cli.ContainerLogs(ctx, c.ID, options)
		if err != nil {
			log.WithFields(log.Fields{
				"containerID": c.ID,
				"error":       err,
			}).Error("Unable to retrieve container logs")
			logFile.Close()
			continue
		}

		log.WithFields(log.Fields{
			"container": sanitizedContainerName,
			"filename":  logsFilename,
		}).Info("Writing docker logs data")

		buf := bufio.NewScanner(logs)
		for buf.Scan() {
			bytes := buf.Bytes()
			var sanitizedBytes []byte

			if len(bytes) > 7 {
				B1 := bytes[0]
				B2 := bytes[1]
				B3 := bytes[2]
				B4 := bytes[3]
				B5 := bytes[4]
				B6 := bytes[5]
				B7 := bytes[6]

				zeroByte := byte(0)

				//Remove header bytes from the docker cli log scans if they match specific patterns.
				if B1 == byte(50) && B2 == byte(48) && B3 == byte(50) && B4 == byte(50) && B5 == byte(47) && B6 == byte(48) && B7 == byte(54) {
					sanitizedBytes = bytes[8:]
				} else if (B1 == byte(2) || B1 == byte(1)) && B2 == zeroByte && B3 == zeroByte && B4 == zeroByte && B5 == zeroByte && B6 == zeroByte && (B7 == zeroByte || B7 == byte(1)) {
					sanitizedBytes = bytes[8:]
				} else {
					sanitizedBytes = bytes
				}
			}

			sanitizedLogLine := string(sanitizedBytes) + "\n"

			if len(strToRedact) > 0 {
				sanitizedLogLine = analyseLogLineForRedaction(sanitizedLogLine)
			}

			_, err = io.Copy(logFile, strings.NewReader(sanitizedLogLine))
			if err != nil {
				log.WithError(err).Error("Unable to write container logs")
				break
			}
		}

		logs.Close()

		if err := logFile.Close(); err != nil {
			log.WithError(err).Error("Unable to close container logs file")
			continue
		}

		filesToZip = append(filesToZip, logsFilename)
	}

	return filesToZip, nil
}

func analyseLogLineForRedaction(line string) string {
	returnLine := strings.ToLower(line)

	for _, v := range strToRedact {
		if strings.Contains(returnLine, strings.ToLower(v)) {
			returnLine = strings.ReplaceAll(returnLine, strings.ToLower(v), "<REDACTED>")
		}
	}

	return returnLine
}

func getKDD() ([]string, error) {
	ctx := context.Background()
	var summaryInfo SummaryInfo
	var finalResponse = make(map[string]interface{})
	var filesToZip []string

	if os.Getenv("KONG_KONNECT_MODE") != "" {
		// avoid reusing native Kong variable names, i.e. KONG_KONNECT_MODE
		// https://docs.konghq.com/gateway/latest/reference/configuration/#konnect_mode
		konnectMode, _ = strconv.ParseBool(os.Getenv("KONG_KDD_KONNECT"))
	}

	if os.Getenv("KONG_ADDR") != "" {
		kongAddr = os.Getenv("KONG_ADDR")
	}

	if os.Getenv("RBAC_HEADER") != "" {
		deckHeaders = strings.Split(os.Getenv("RBAC_HEADER"), ",")
	}

	if !konnectMode {
		// Get the Kong client
		client, err := utils.GetKongClient(utils.KongClientConfig{
			Address: kongAddr,
			TLSConfig: utils.TLSConfig{
				SkipVerify: true,
			},
			Debug:   false,
			Headers: deckHeaders,
		})

		if err != nil {
			log.WithError(err).Warn("Failed to get Kong client, skipping KDD collection")
			return filesToZip, nil
		}

		// response of GET request on the root of the Admin
		root, err := client.RootJSON(context.Background())
		if err != nil {
			log.WithError(err).Warn("Failed to get root JSON from Kong, skipping KDD collection")
			return filesToZip, nil
		}

		// create a map from the JSON response
		rootConfig, err = objx.FromJSON(string(root))
		if err != nil {
			log.WithError(err).Warn("Failed to parse root JSON, skipping KDD collection")
			return filesToZip, nil
		}

		// Get the status and list of workspaces
		status, err := getEndpoint(client, "/status")
		if err != nil {
			log.WithError(err).Warn("Failed to get status endpoint")
		}

		workspaces, err := getWorkspaces(client)
		if err != nil {
			log.WithError(err).Warn("Failed to get workspaces, skipping KDD collection")
			return filesToZip, nil
		}

		// Get the license report
		licenseReport, err := getEndpoint(client, "/license/report")
		if err != nil {
			log.WithError(err).Warn("Failed to get license report")
		}

		// Update the summaryInfo struct
		summaryInfo.TotalWorkspaceCount = len(workspaces.Data)
		summaryInfo.DeploymentTopology = rootConfig.Get("configuration.role").Str()
		summaryInfo.DatabaseType = rootConfig.Get("configuration.database").Str()
		summaryInfo.KongVersion = rootConfig.Get("version").Str()

		switch summaryInfo.DeploymentTopology {
		case "control_plane":
			summaryInfo.DeploymentTopology = "hybrid"
		case "traditional":
			if summaryInfo.DatabaseType == "off" {
				summaryInfo.DeploymentTopology = "DB-Less"
			}
		}

		// Add the root config, status, and license report to the final response map
		finalResponse["root_config"] = rootConfig
		finalResponse["status"] = status
		finalResponse["license_report"] = licenseReport

		//Incomplete data as yet, but saving what we've collected so far incase of error during workspace iteration
		finalResponse["summary_info"] = summaryInfo

		if os.Getenv("DUMP_WORKSPACE_CONFIGS") != "" {
			log.WithField("env_var", os.Getenv("DUMP_WORKSPACE_CONFIGS")).Debug("Dump workspace configs environment variable found")
			createWorkspaceConfigDumps = (os.Getenv("DUMP_WORKSPACE_CONFIGS") == "true")
		}

		// Process workspaces in parallel with controlled concurrency
		var wg sync.WaitGroup
		var mu sync.Mutex
		// Limit concurrent workspace processing to 5 to avoid overwhelming the API
		semaphore := make(chan struct{}, 5)

		for _, ws := range workspaces.Data {
			wg.Add(1)
			go func(workspace struct {
				Comment interface{} `json:"comment"`
				Config  struct {
					Meta                      interface{} `json:"meta"`
					Portal                    bool        `json:"portal"`
					PortalAccessRequestEmail  interface{} `json:"portal_access_request_email"`
					PortalApprovedEmail       interface{} `json:"portal_approved_email"`
					PortalAuth                interface{} `json:"portal_auth"`
					PortalAuthConf            interface{} `json:"portal_auth_conf"`
					PortalAutoApprove         interface{} `json:"portal_auto_approve"`
					PortalCorsOrigins         interface{} `json:"portal_cors_origins"`
					PortalDeveloperMetaFields string      `json:"portal_developer_meta_fields"`
					PortalEmailsFrom          interface{} `json:"portal_emails_from"`
					PortalEmailsReplyTo       interface{} `json:"portal_emails_reply_to"`
					PortalInviteEmail         interface{} `json:"portal_invite_email"`
					PortalIsLegacy            interface{} `json:"portal_is_legacy"`
					PortalResetEmail          interface{} `json:"portal_reset_email"`
					PortalResetSuccessEmail   interface{} `json:"portal_reset_success_email"`
					PortalSessionConf         interface{} `json:"portal_session_conf"`
					PortalTokenExp            interface{} `json:"portal_token_exp"`
				} `json:"config"`
				CreatedAt int    `json:"created_at"`
				ID        string `json:"id"`
				Meta      struct {
					Color     string      `json:"color"`
					Thumbnail interface{} `json:"thumbnail"`
				} `json:"meta"`
				Name string `json:"name"`
			}) {
				defer wg.Done()
				semaphore <- struct{}{}        // Acquire semaphore
				defer func() { <-semaphore }() // Release semaphore

				log.WithField("workspace", workspace.Name).Info("Processing workspace")

				// Create a workspace-specific client to avoid race conditions
				wsClient, err := utils.GetKongClient(utils.KongClientConfig{
					Address: kongAddr,
					TLSConfig: utils.TLSConfig{
						SkipVerify: true,
					},
					Debug:   false,
					Headers: deckHeaders,
				})
				if err != nil {
					log.WithFields(log.Fields{
						"workspace": workspace.Name,
						"error":     err,
					}).Error("Failed to get Kong client for workspace")
					return
				}

				wsClient.SetWorkspace(workspace.Name)

				// Queries all the entities using client and returns all the entities in KongRawState.
				d, err := dump.Get(context.Background(), wsClient, dump.Config{
					RBACResourcesOnly: false,
					SkipConsumers:     false,
				})

				if err != nil {
					log.WithFields(log.Fields{
						"workspace": workspace.Name,
						"error":     err,
					}).Error("Error getting workspace data, continuing to next workspace")
					return
				}

				// Count regex routes
				regexRouteCount := 0
				for _, v := range d.Routes {
					for _, route := range v.Paths {
						if strings.HasPrefix(*route, "~") {
							regexRouteCount++
						}
					}
				}

				// Check if portal is enabled
				portalEnabled := 0
				if workspace.Config.Portal {
					portalEnabled = 1
				}

				// Update summary info with mutex protection
				mu.Lock()
				summaryInfo.TotalConsumerCount += len(d.Consumers)
				summaryInfo.TotalServiceCount += len(d.Services)
				summaryInfo.TotalRouteCount += len(d.Routes)
				summaryInfo.TotalPluginCount += len(d.Plugins)
				summaryInfo.TotalTargetCount += len(d.Targets)
				summaryInfo.TotalUpstreamCount += len(d.Upstreams)
				summaryInfo.TotalRegExRoutes += regexRouteCount
				summaryInfo.TotalEnabledDevPortalCount += portalEnabled
				mu.Unlock()

				if createWorkspaceConfigDumps {
					ks, err := state.Get(d)
					if err != nil {
						log.WithFields(log.Fields{
							"workspace": workspace.Name,
							"error":     err,
						}).Error("Error building Kong dump state")
						return
					}

					err = file.KongStateToFile(ks, file.WriteConfig{
						KongVersion: summaryInfo.KongVersion,
						Filename:    workspace.Name + "-kong-dump.yaml",
						FileFormat:  file.YAML,
					})
					if err != nil {
						log.WithFields(log.Fields{
							"workspace": workspace.Name,
							"error":     err,
						}).Error("Error building Kong dump file")
					} else {
						log.WithField("workspace", workspace.Name).Info("Successfully dumped workspace")
						mu.Lock()
						filesToZip = append(filesToZip, workspace.Name+"-kong-dump.yaml")
						mu.Unlock()
					}
				}
			}(ws)
		}

		// Wait for all workspaces to be processed
		wg.Wait()

		// Add the full info now we know we have it all
		finalResponse["summary_info"] = summaryInfo

		jsonBytes, err := json.Marshal(finalResponse)
		if err != nil {
			log.WithError(err).Error("Error marshalling KDD data to JSON")
			return filesToZip, err
		}

		err = os.WriteFile("KDD.json", jsonBytes, 0644)
		if err != nil {
			log.WithError(err).Fatal("Error writing KDD.json")
			return filesToZip, err
		}

		filesToZip = append(filesToZip, "KDD.json")
		return filesToZip, nil
	}

	// Handle Konnect mode
	log.Info("Running in Konnect mode")
	httpClient := utils.HTTPClient()

	// Setup the Konnect client
	log.WithField("deckHeaders", deckHeaders).Debug("Using deck headers")
	config := utils.KonnectConfig{
		ControlPlaneName: controlPlaneName,
		Token:            deckHeaders[0],
		Address:          kongAddr,
	}

	// Tack the token on as an auth header
	if config.Token != "" {
		config.Headers = append(
			config.Headers, "Authorization:Bearer "+config.Token,
		)
	}

	client, err := utils.GetKonnectClient(httpClient, config)
	if err != nil {
		log.WithError(err).Error("Failed to get Konnect client")
		return nil, err
	}

	// Before we do anything, we need to login
	authResponse, err := client.Auth.LoginV2(ctx, config.Email, config.Password, config.Token)
	if err != nil {
		log.WithError(err).Error("Failed to login to Konnect")
		return nil, err
	}

	log.WithFields(log.Fields{
		"name":     authResponse.Name,
		"orgID":    authResponse.OrganizationID,
		"org":      authResponse.Organization,
		"fullName": authResponse.FullName,
	}).Debug("Authenticated with Konnect")

	var listOpt *konnect.ListOpt
	controlPlanes, _, err := client.RuntimeGroups.List(ctx, listOpt)
	if err != nil {
		log.WithError(err).Error("Failed to list control planes")
		return nil, err
	}

	var cpID string
	for _, controlPlane := range controlPlanes {
		if *controlPlane.Name == controlPlaneName {
			cpID = *controlPlane.ID
			log.WithFields(log.Fields{
				"name": controlPlaneName,
				"id":   cpID,
			}).Info("Found control plane")
		}
	}

	if cpID == "" {
		log.WithField("controlPlaneName", controlPlaneName).Error("Control plane not found")
		return nil, fmt.Errorf("control plane %s not found", controlPlaneName)
	}

	konnectAddress := kongAddr + "/v2/control-planes/" + cpID + "/core-entities"
	kongClient, err := utils.GetKongClient(utils.KongClientConfig{
		Address:    konnectAddress,
		HTTPClient: httpClient,
		Debug:      config.Debug,
		Headers:    config.Headers,
		Retryable:  true,
		TLSConfig:  config.TLSConfig,
	})

	if err != nil {
		log.WithError(err).Error("Failed to get Kong client for Konnect")
		return nil, err
	}

	dumpConfig.KonnectControlPlane = controlPlaneName
	rawState, err := dump.Get(ctx, kongClient, dumpConfig)
	if err != nil {
		log.WithError(err).Error("Failed reading configuration from Kong")
		return nil, fmt.Errorf("Failed reading configuration from Kong: %w", err)
	}

	summaryInfo.TotalConsumerCount = len(rawState.Consumers)
	summaryInfo.TotalServiceCount = len(rawState.Services)
	summaryInfo.TotalRouteCount = len(rawState.Routes)
	summaryInfo.TotalPluginCount = len(rawState.Plugins)
	summaryInfo.TotalTargetCount = len(rawState.Targets)
	summaryInfo.TotalUpstreamCount = len(rawState.Upstreams)

	ks, err := state.Get(rawState)
	if err != nil {
		log.WithError(err).Error("Failed building state")
		return nil, fmt.Errorf("building state: %w", err)
	}

	filename := "konnect-" + controlPlaneName + ".yaml"
	err = file.KongStateToFile(ks, file.WriteConfig{
		SelectTags:       dumpConfig.SelectorTags,
		Filename:         filename,
		FileFormat:       file.YAML,
		WithID:           true,
		ControlPlaneName: controlPlaneName,
		KongVersion:      "3.5.0.0", // placeholder
	})

	if err != nil {
		log.WithFields(log.Fields{
			"controlPlane": controlPlaneName,
			"error":        err,
		}).Error("Failed building Kong dump file")
	} else {
		log.WithField("controlPlane", controlPlaneName).Info("Successfully dumped Control Plane")
		filesToZip = append(filesToZip, filename)
	}

	finalResponse["summary_info"] = summaryInfo

	jsonBytes, err := json.Marshal(finalResponse)
	if err != nil {
		log.WithError(err).Error("Error marshalling JSON")
		return nil, err
	}

	err = os.WriteFile("KDD.json", jsonBytes, 0644)
	if err != nil {
		log.WithError(err).Fatal("Error writing KDD.json")
		return filesToZip, err
	}

	filesToZip = append(filesToZip, "KDD.json")
	return filesToZip, nil
}

func getEndpoint(client *kong.Client, endpoint string) (objx.Map, error) {
	req, err := client.NewRequest("GET", endpoint, nil, nil)
	if err != nil {
		return nil, err
	}

	oReturn, err := getObjx(req, client)
	if err != nil {
		return nil, err
	}

	return oReturn, nil
}

func getObjx(req *http.Request, client *kong.Client) (objx.Map, error) {
	resp, err := client.DoRAW(context.Background(), req)
	if err != nil {
		return nil, err
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	strBody := string(body)
	oReturn, err := objx.FromJSON(strBody)
	if err != nil {
		return nil, err
	}

	return oReturn, nil
}

func getWorkspaces(client *kong.Client) (*Workspaces, error) {
	req, err := client.NewRequest("GET", "/workspaces", nil, nil)
	if err != nil {
		return nil, err
	}

	var w Workspaces
	_, err = client.Do(context.Background(), req, &w)
	if err != nil {
		return nil, err
	}
	return &w, nil
}

func runKubernetes(filesToCopy []string, commandsToRun []NamedCommand) ([]string, error) {
	log.Info("Running Kubernetes collection")
	ctx := context.Background()
	// Pre-allocate with reasonable capacity
	kongK8sPods := make([]corev1.Pod, 0, 20)
	filesToZip := make([]string, 0, 50)

	kubeClient, clientConfig, err := createClient()
	if err != nil {
		log.WithError(err).Error("Unable to create k8s client")
		return nil, err
	}

	pl, err := kubeClient.CoreV1().Pods("").List(ctx, v1.ListOptions{})
	if err != nil {
		log.WithError(err).Error("Failed to list pods")
		return nil, err
	}

	if os.Getenv("TARGET_PODS") != "" {
		targetPods = strings.Split(os.Getenv("TARGET_PODS"), ",")
		log.WithField("targetPods", targetPods).Info("Using target pods from environment")
	}

	// To keep track of whether a particular pod has been added already.
	foundPod := make(map[string]bool)

	for _, p := range pl.Items {
		if len(targetPods) > 0 {
			for _, podName := range targetPods {
				if strings.ToLower(podName) == strings.ToLower(p.Name) {
					for _, c := range p.Spec.Containers {
						for _, i := range kongImages {
							if strings.Contains(c.Image, i) {
								if !foundPod[p.Name] {
									log.WithFields(log.Fields{
										"pod":            p.Name,
										"containerCount": len(p.Spec.Containers),
									}).Info("Found target pod")
									kongK8sPods = append(kongK8sPods, p)
									foundPod[p.Name] = true
								}
							}
						}
					}
				}
			}
		} else {
			for _, c := range p.Spec.Containers {
				for _, i := range kongImages {
					if strings.Contains(c.Image, i) {
						if !foundPod[p.Name] {
							log.WithFields(log.Fields{
								"pod":            p.Name,
								"containerCount": len(p.Spec.Containers),
							}).Info("Found Kong pod")
							kongK8sPods = append(kongK8sPods, p)
							foundPod[p.Name] = true
						}
					}
				}
			}
		}
	}

	if len(kongK8sPods) > 0 {
		log.WithField("podCount", len(kongK8sPods)).Info("Processing Kubernetes pods")

		logFilenames, err := writePodDetails(ctx, kubeClient, kongK8sPods)
		if err != nil {
			log.WithError(err).Error("Error writing pod details")
		} else {
			filesToZip = append(filesToZip, logFilenames...)
		}

		// Process pods in parallel with controlled concurrency
		var wg sync.WaitGroup
		var mu sync.Mutex
		// Limit concurrent pod processing to 10 to avoid overwhelming the API server
		semaphore := make(chan struct{}, 10)

		for _, pod := range kongK8sPods {
			wg.Add(1)
			go func(p corev1.Pod) {
				defer wg.Done()
				semaphore <- struct{}{}        // Acquire semaphore
				defer func() { <-semaphore }() // Release semaphore

				log.WithFields(log.Fields{
					"pod":       p.Name,
					"namespace": p.Namespace,
				}).Info("Processing pod")

				var podFiles []string

				for _, container := range p.Spec.Containers {
					relevantImage := false
					for _, i := range kongImages {
						if strings.Contains(container.Image, i) {
							relevantImage = true
							break
						}
					}

					if !relevantImage {
						continue
					}

					log.WithField("container", container.Name).Info("Processing container")

					for _, file := range filesToCopy {
						namedCmd := NamedCommand{
							Cmd:  []string{"cat", file},
							Name: file,
						}

						filename, err := RunCommandInPod(ctx, kubeClient, clientConfig, p.Namespace, p.Name, container.Name, namedCmd)
						if err != nil {
							log.WithFields(log.Fields{
								"pod":       p.Name,
								"container": container.Name,
								"file":      file,
								"error":     err,
							}).Error("Error copying file from pod")
						} else if filename != "" {
							log.WithFields(log.Fields{
								"pod":      p.Name,
								"file":     file,
								"filename": filename,
							}).Debug("Copied file from pod")
							podFiles = append(podFiles, filename)
						}
					}

					for _, namedCmd := range commandsToRun {
						filename, err := RunCommandInPod(ctx, kubeClient, clientConfig, p.Namespace, p.Name, container.Name, namedCmd)
						if err != nil {
							log.WithFields(log.Fields{
								"pod":       p.Name,
								"container": container.Name,
								"command":   strings.Join(namedCmd.Cmd, " "),
								"error":     err,
							}).Error("Error running command in pod")
						} else if filename != "" {
							log.WithFields(log.Fields{
								"pod":      p.Name,
								"command":  strings.Join(namedCmd.Cmd, " "),
								"filename": filename,
							}).Debug("Command executed in pod")
							podFiles = append(podFiles, filename)
						}
					}
				}

				// Add collected files to the shared filesToZip slice with mutex protection
				if len(podFiles) > 0 {
					mu.Lock()
					filesToZip = append(filesToZip, podFiles...)
					mu.Unlock()
				}
			}(pod)
		}

		// Wait for all pods to be processed
		wg.Wait()
	} else {
		log.Warn("No pods with the appropriate container images found in cluster")
	}

	return filesToZip, nil
}

func createAndWriteLogFile(initialLogName string, contents string) (string, error) {
	hostname, _ := os.Hostname()
	logName := fmt.Sprintf(hostname+"_"+initialLogName+"-%s.log", time.Now().Format("2006-01-02-15-04-05"))

	logFile, err := os.Create(logName)
	if err != nil {
		log.WithFields(log.Fields{
			"initialLogName": initialLogName,
			"error":          err,
		}).Error("Cannot create log file")
		return "", err
	}

	defer logFile.Close()

	if _, err = io.Copy(logFile, strings.NewReader(contents)); err != nil {
		log.WithFields(log.Fields{
			"initialLogName": initialLogName,
			"error":          err,
		}).Error("Unable to write contents to log file")
		return "", err
	}

	return logName, nil
}

func runVM() ([]string, error) {
	log.Info("Running in VM mode")

	if lineLimit == LineLimitDefault {
		log.WithField("lineLimit", LineLimitDefault).Info("Using default line limit value")
	}

	var filesToZip []string

	filesToCopy := [][2]string{
		{"/etc/resolv.conf", vmResolvFile},
		{"/etc/hosts", vmHostsFile},
	}

	if prefixDir != "" {
		log.WithField("prefixDir", prefixDir).Info("Reading environment file")

		d, err := os.ReadFile(prefixDir + "/.kong_env")
		if err != nil {
			log.WithError(err).Error("Error reading config file")
			return nil, err
		}

		configSummary, err := os.Create("vm-kong-env.txt")
		if err != nil {
			log.WithError(err).Error("Error creating vm-kong-env.txt")
			return nil, err
		}

		log.Info("Writing kong environment data")
		if _, err = io.Copy(configSummary, bytes.NewReader(d)); err != nil {
			log.WithError(err).Error("Error writing kong environment data")
			configSummary.Close()
			return nil, err
		}

		if err = configSummary.Close(); err != nil {
			log.WithError(err).Error("Error closing vm-kong-env.txt")
			return nil, err
		}

		filesToZip = append(filesToZip, "vm-kong-env.txt")

		// Collect VM resources in parallel
		type resourceTask struct {
			function     func() (interface{}, error)
			resourceType string
			logFile      string
		}

		tasks := []resourceTask{
			{RetrieveVMMemoryInfo, "memory", vmMemoryLogFile},
			{RetrieveVMCPUInfo, "cpu", vmCPULogFile},
			{RetrieveVMDiskInfo, "disk", vmDiskLogFile},
			{RetrieveProcessInfo, "process", vmProcessLogFile},
			{RetrieveNetworkInfo, "network", vmNetworkLogFile},
		}

		var wg sync.WaitGroup
		var mu sync.Mutex
		resourceFiles := make([]string, 0, len(tasks))

		for _, task := range tasks {
			wg.Add(1)
			go func(t resourceTask) {
				defer wg.Done()

				if err := getResourceAndMarshall(t.function, t.resourceType, t.logFile); err != nil {
					log.WithFields(log.Fields{
						"resourceType": t.resourceType,
						"error":        err,
					}).Error("Error retrieving resource info")
				} else {
					mu.Lock()
					resourceFiles = append(resourceFiles, t.logFile)
					mu.Unlock()
				}
			}(task)
		}

		// Wait for all resource collection to complete
		wg.Wait()

		// Add all successfully collected resource files
		filesToZip = append(filesToZip, resourceFiles...)

		for _, v := range filesToCopy {
			if err := copyFiles(v[0], v[1]); err != nil {
				log.WithFields(log.Fields{
					"src":   v[0],
					"dst":   v[1],
					"error": err,
				}).Error("Error copying file")
			} else {
				filesToZip = append(filesToZip, v[1])
			}
		}

		// Config keys that have the paths to log files that need extracting
		configKeys := []string{"admin_access_log", "admin_error_log", "proxy_access_log", "proxy_error_log"}

		for _, v := range configKeys {
			logName := collectAndLimitLog(string(d), v)
			if logName != "" {
				filesToZip = append(filesToZip, logName)
			}
		}
	} else {
		log.Warn("No prefix directory set. The prefix parameter must be set for VM log extraction.")
	}

	return filesToZip, nil
}

func getResourceAndMarshall(functionName func() (interface{}, error), resourceType string, logFile string) error {
	log.WithFields(log.Fields{
		"resourceType": resourceType,
		"logFile":      logFile,
	}).Debug("Retrieving and marshalling resource")

	resource, err := functionName()
	if err != nil {
		return err
	}

	infoJSON, err := json.Marshal(resource)
	if err != nil {
		return err
	}

	err = os.WriteFile(logFile, infoJSON, 0644)
	if err != nil {
		return err
	}

	return nil
}

func collectAndLimitLog(envars, configKey string) string {
	log.WithField("configKey", configKey).Debug("Collecting and limiting log")

	splitEnvars := strings.Split(envars, "\n")

	for _, configLine := range splitEnvars {
		if strings.Contains(configLine, configKey) {
			logPath := getConfigValue(configLine)

			if logPath == "" {
				log.WithField("configKey", configKey).Warn("Empty log path found, skipping")
				continue
			}

			var logLines []string

			if logPath[:4] == "logs" {
				fullLogPath := prefixDir + "/" + logPath
				log.WithFields(log.Fields{
					"prefix":   prefixDir,
					"logPath":  logPath,
					"fullPath": fullLogPath,
				}).Debug("Using prefix for log path")
				logPath = fullLogPath
			}

			// Get file length in bytes
			logLength := getFileLength(logPath)
			if logLength <= 0 {
				log.WithField("logPath", logPath).Info("Log file has no length, continuing...")
				continue
			}

			log.WithFields(log.Fields{
				"path":   logPath,
				"length": logLength,
			}).Debug("Log file information")

			logFile, err := os.Open(logPath)
			if err != nil {
				log.WithFields(log.Fields{
					"logPath": logPath,
					"error":   err,
				}).Error("Error opening log")
				continue
			}

			defer logFile.Close()

			// Use buffered reading for better performance
			const bufferSize = 64 * 1024 // 64KB buffer
			buffer := make([]byte, bufferSize)
			var singleLineBytes []byte
			linesProcessed := int64(0)
			bytesProcessed := int64(0)
			success := false

			// Read backwards in chunks
			for bytesProcessed < logLength && linesProcessed < lineLimit {
				// Calculate how much to read
				chunkSize := int64(bufferSize)
				readPos := logLength - bytesProcessed - chunkSize
				if readPos < 0 {
					chunkSize += readPos
					readPos = 0
				}

				// Read a chunk
				n, err := logFile.ReadAt(buffer[:chunkSize], readPos)
				if err != nil && err != io.EOF {
					log.WithFields(log.Fields{
						"logPath": logPath,
						"error":   err,
					}).Error("Unable to read from log file")
					break
				}

				// Process the chunk backwards
				for i := n - 1; i >= 0 && linesProcessed < lineLimit; i-- {
					lastReadByte := buffer[i]
					bytesProcessed++

					// Check for \n byte
					if lastReadByte == 10 {
						// Reverse the line since we're reading backwards
						for j, k := 0, len(singleLineBytes)-1; j < k; j, k = j+1, k-1 {
							singleLineBytes[j], singleLineBytes[k] = singleLineBytes[k], singleLineBytes[j]
						}

						logLines = append(logLines, string(singleLineBytes))
						singleLineBytes = singleLineBytes[:0] // Reuse slice
						linesProcessed++
						success = true
					} else {
						singleLineBytes = append(singleLineBytes, lastReadByte)
					}
				}

				if bytesProcessed >= logLength || linesProcessed >= lineLimit {
					break
				}
			}

			if success {
				// Flip the lines as they are read backwards
				for i, j := 0, len(logLines)-1; i < j; i, j = i+1, j-1 {
					logLines[i], logLines[j] = logLines[j], logLines[i]
				}

				sanitizedLogLines := logLines
				if len(strToRedact) > 0 {
					for i, v := range logLines {
						sanitizedLogLines[i] = analyseLogLineForRedaction(v)
					}
				}

				concatLogs := strings.Join(sanitizedLogLines, "\n")
				if len(concatLogs) > 0 {
					logName, err := createAndWriteLogFile(configKey, concatLogs)
					if err != nil {
						log.WithFields(log.Fields{
							"configKey": configKey,
							"error":     err,
						}).Error("Error creating or writing log file")
					} else {
						log.WithFields(log.Fields{
							"configKey": configKey,
							"logName":   logName,
						}).Info("Log file successfully created")
						return logName
					}
				} else {
					log.WithField("configKey", configKey).Info("Skipping creation of logs as the log either does not exist or has no length")
				}

				log.WithFields(log.Fields{
					"configKey":  configKey,
					"linesCount": len(sanitizedLogLines),
				}).Info("Finished reading log")
			}

			logFile.Close()
		}
	}

	return ""
}

func getConfigValue(entry string) string {
	aEntry := strings.Split(entry, "=")
	if len(aEntry) < 2 {
		return ""
	}
	return strings.Trim(aEntry[1], " ")
}

// Returns total length of byte array
func getFileLength(logPath string) int64 {
	log.WithField("path", logPath).Debug("Getting log file length")
	size := int64(0)

	fileInfo, err := os.Stat(logPath)
	if err != nil {
		log.WithFields(log.Fields{
			"path":  logPath,
			"error": err,
		}).Error("Error reading file info")
		return 0
	}

	size = fileInfo.Size()
	log.WithFields(log.Fields{
		"path": logPath,
		"size": size,
	}).Debug("File size determined")

	return size
}

func createClient() (kubernetes.Interface, *rest.Config, error) {
	log.Debug("Creating Kubernetes client")
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	)

	clientConfig, err := kubeConfig.ClientConfig()
	if err != nil {
		return nil, nil, errors.Wrap(err, "error finding Kubernetes API server config in --kubeconfig, $KUBECONFIG, or in-cluster configuration")
	}

	clientSet, err := kubernetes.NewForConfig(clientConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to create a client: %v", err)
	}

	return clientSet, clientConfig, nil
}

func writePodDetails(ctx context.Context, clientSet kubernetes.Interface, podList []corev1.Pod) ([]string, error) {
	// Pre-allocate with estimated capacity (2 files per pod: logs + yaml)
	logFilenames := make([]string, 0, len(podList)*2)

	for _, pod := range podList {
		p, err := clientSet.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		if err != nil {
			log.WithFields(log.Fields{
				"pod":       pod.Name,
				"namespace": pod.Namespace,
				"error":     err,
			}).Error("Error getting pod details")
			continue
		}

		log.WithFields(log.Fields{
			"pod":       p.Name,
			"namespace": p.Namespace,
		}).Info("Processing pod details")

		for _, container := range p.Spec.Containers {
			relevantImage := false
			for _, i := range kongImages {
				if strings.Contains(container.Image, i) {
					relevantImage = true
					break
				}
			}

			if !relevantImage {
				continue
			}

			log.WithField("container", container.Name).Info("Processing container logs")

			if os.Getenv("K8S_LOGS_SINCE_SECONDS") != "" {
				var err error
				logsSinceSeconds, err = strconv.ParseInt(os.Getenv("K8S_LOGS_SINCE_SECONDS"), 10, 64)
				if err != nil {
					log.WithError(err).Warn("Invalid K8S_LOGS_SINCE_SECONDS value, using default")
				}
			}

			podLogOpts := corev1.PodLogOptions{Container: container.Name}
			if logsSinceSeconds > 0 {
				podLogOpts.SinceSeconds = &logsSinceSeconds
				log.WithField("sinceSeconds", logsSinceSeconds).Debug("Using time-based log retrieval")
			} else {
				podLogOpts.TailLines = &lineLimit
				log.WithField("tailLines", lineLimit).Debug("Using line-based log retrieval")
			}

			podLogs, err := clientSet.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &podLogOpts).Stream(ctx)
			if err != nil {
				log.WithFields(log.Fields{
					"pod":       pod.Name,
					"container": container.Name,
					"error":     err,
				}).Error("Error retrieving pod logs")
				continue
			}

			sanitizedImageName := strings.ReplaceAll(strings.ReplaceAll(container.Image, ":", "/"), "/", "-")
			logsFilename := fmt.Sprintf("%s-%s.log", pod.Name, sanitizedImageName)

			logFile, err := os.Create(logsFilename)
			if err != nil {
				log.WithFields(log.Fields{
					"filename": logsFilename,
					"error":    err,
				}).Error("Error creating log file")
				podLogs.Close()
				continue
			}

			if len(strToRedact) > 0 {
				buf := bufio.NewScanner(podLogs)
				for buf.Scan() {
					bytes := buf.Bytes()
					sanitizedLogLine := analyseLogLineForRedaction(string(bytes) + "\n")
					if _, err := io.Copy(logFile, strings.NewReader(sanitizedLogLine)); err != nil {
						log.WithError(err).Error("Unable to write container logs")
						break
					}
				}
			} else {
				if _, err := io.Copy(logFile, podLogs); err != nil {
					log.WithError(err).Error("Error copying pod logs to file")
					logFile.Close()
					podLogs.Close()
					continue
				}
			}

			podLogs.Close()
			logFile.Close()
			logFilenames = append(logFilenames, logsFilename)
		}

		podDefFileName := fmt.Sprintf("%s.yaml", p.Name)
		podDefFile, err := os.Create(podDefFileName)
		if err != nil {
			log.WithFields(log.Fields{
				"filename": podDefFileName,
				"error":    err,
			}).Error("Error creating pod definition file")
			continue
		}

		buf := bytes.NewBufferString("")
		pod.TypeMeta = metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		}

		scheme := runtime.NewScheme()
		serializer := kjson.NewSerializerWithOptions(
			kjson.DefaultMetaFactory,
			scheme,
			scheme,
			kjson.SerializerOptions{
				Pretty: true,
				Yaml:   true,
				Strict: true,
			},
		)

		err = serializer.Encode(&pod, buf)
		if err != nil {
			log.WithError(err).Error("Error encoding pod definition")
			podDefFile.Close()
			continue
		}

		_, err = io.Copy(podDefFile, buf)
		if err != nil {
			log.WithError(err).Error("Error writing pod definition")
			podDefFile.Close()
			continue
		}

		podDefFile.Close()
		logFilenames = append(logFilenames, podDefFileName)
	}

	return logFilenames, nil
}

func writeFiles(filesToWrite []string) error {
	if len(filesToWrite) == 0 {
		log.Warn("No files to write to archive")
		return nil
	}

	outputName := fmt.Sprintf("%s-support.tar.gz", time.Now().Format("2006-01-02-15-04-05"))
	log.WithField("filename", outputName).Info("Creating archive")

	output, err := os.Create(outputName)
	if err != nil {
		log.WithError(err).Error("Failed to create output file")
		return err
	}

	defer func() {
		if err := output.Close(); err != nil {
			log.WithError(err).Error("Error closing output file")
		}
	}()

	// Create the archive and write the output
	gw := gzip.NewWriter(output)
	defer func() {
		if err := gw.Close(); err != nil {
			log.WithError(err).Error("Error closing gzip writer")
		}
	}()

	tw := tar.NewWriter(gw)
	defer func() {
		if err := tw.Close(); err != nil {
			log.WithError(err).Error("Error closing tar writer")
		}
	}()

	// Iterate over files and add them to the tar archive
	for _, file := range filesToWrite {
		err := addToArchive(tw, file)
		if err != nil {
			log.WithFields(log.Fields{
				"file":  file,
				"error": err,
			}).Error("Error adding file to archive")
			return err
		}
	}

	log.WithField("filename", output.Name()).Info("Diagnostics have been written to archive")

	err = cleanupFiles(filesToWrite)
	if err != nil {
		log.WithError(err).Error("Error cleaning up files")
	}

	return nil
}

func addToArchive(tw *tar.Writer, filename string) error {
	log.WithField("filename", filename).Debug("Adding file to archive")

	// Open the file which will be written into the archive
	file, err := os.Open(filename)
	if err != nil {
		return err
	}

	defer func() {
		if err := file.Close(); err != nil {
			log.WithFields(log.Fields{
				"filename": filename,
				"error":    err,
			}).Error("Error closing file")
		}
	}()

	// Get FileInfo about our file providing file size, mode, etc.
	info, err := file.Stat()
	if err != nil {
		return err
	}

	// Create a tar Header from the FileInfo data
	header, err := tar.FileInfoHeader(info, info.Name())
	if err != nil {
		return err
	}

	// Use full path as name (FileInfoHeader only takes the basename)
	header.Name = filename

	// Write file header to the tar archive
	err = tw.WriteHeader(header)
	if err != nil {
		return err
	}

	// Copy file content to tar archive
	_, err = io.Copy(tw, file)
	if err != nil {
		return err
	}

	return nil
}

func roundToTwoDecimals(num float64) float64 {
	return math.Round(num*100) / 100
}

func RetrieveNetworkInfo() (interface{}, error) {
	log.Debug("Retrieving network information")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Pre-allocate with reasonable capacity
	netStats := make([]NetworkInfo, 0, 100)
	conn, err := net.ConnectionsWithContext(ctx, "all")

	if err != nil {
		log.WithError(err).Error("Failed to retrieve network connections")
		return NetworkInfo{}, err
	}

	for _, v := range conn {
		netStats = append(netStats, NetworkInfo{
			Fd:     v.Fd,
			Family: v.Family,
			Type:   v.Type,
			Laddr:  v.Laddr.IP,
			Raddr:  v.Raddr.IP,
			Status: v.Status,
			Pid:    v.Pid,
			Uids:   v.Uids,
		})
	}

	return netStats, nil
}

func RetrieveProcessInfo() (interface{}, error) {
	log.Debug("Retrieving process information")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Pre-allocate with reasonable capacity
	ps := make([]ProcessInfo, 0, 100)
	processes, err := process.ProcessesWithContext(ctx)

	if err != nil {
		log.WithError(err).Error("Failed to retrieve processes")
		return ProcessInfo{}, err
	}

	for _, p := range processes {
		name, err := p.NameWithContext(ctx)
		if err != nil {
			log.WithFields(log.Fields{
				"pid":   p.Pid,
				"error": err,
			}).Debug("Failed to get process name")
			continue
		}

		pid := p.Pid
		cpuPercent, err := p.CPUPercentWithContext(ctx)
		if err != nil {
			log.WithFields(log.Fields{
				"pid":   p.Pid,
				"error": err,
			}).Debug("Failed to get process CPU usage")
			continue
		}

		memInfo, err := p.MemoryInfoWithContext(ctx)
		if err != nil {
			log.WithFields(log.Fields{
				"pid":   p.Pid,
				"error": err,
			}).Debug("Failed to get process memory info")
			continue
		}

		cmdLine, err := p.CmdlineWithContext(ctx)
		if err != nil {
			log.WithFields(log.Fields{
				"pid":   p.Pid,
				"error": err,
			}).Debug("Failed to get process command line")
			continue
		}

		ps = append(ps, ProcessInfo{
			PID:        pid,
			Name:       name,
			CPUPercent: fmt.Sprintf("%.2f", cpuPercent),
			MemPercent: memInfo.RSS,
			CmdLine:    cmdLine,
		})
	}
	return ps, nil
}

func RetrieveVMDiskInfo() (interface{}, error) {
	log.Debug("Retrieving VM disk information")
	var bytesToGB uint64 = 1024 * 1024 * 1024
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	diskinfo, err := disk.UsageWithContext(ctx, "/")
	if err != nil {
		log.WithError(err).Error("Failed to retrieve disk usage")
		return DiskInfo{}, err
	}

	return DiskInfo{
		Total:       diskinfo.Total / bytesToGB,
		Free:        diskinfo.Free / bytesToGB,
		Used:        diskinfo.Used / bytesToGB,
		UsedPercent: diskinfo.UsedPercent,
	}, nil
}

func RetrieveVMCPUInfo() (interface{}, error) {
	log.Debug("Retrieving VM CPU information")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cpuinfo, err := cpu.InfoWithContext(ctx)
	if err != nil {
		log.WithError(err).Error("Failed to retrieve CPU information")
		return CPUInfo{}, err
	}

	if len(cpuinfo) == 0 {
		log.Warn("No CPU info retrieved")
		return CPUInfo{}, fmt.Errorf("no CPU information available")
	}

	return CPUInfo{
		CPU:   len(cpuinfo),
		Cores: cpuinfo[0].Cores,
	}, nil
}

func RetrieveVMMemoryInfo() (interface{}, error) {
	log.Debug("Retrieving VM memory information")
	bytesToGB := 1024.0 * 1024.0 * 1024.0
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	memInfo, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		log.WithError(err).Error("Failed to retrieve virtual memory info")
		return MemoryInfo{}, err
	}

	swapMem, err := mem.SwapMemory()
	if err != nil {
		log.WithError(err).Error("Failed to retrieve swap memory info")
		return MemoryInfo{}, err
	}

	return MemoryInfo{
		PhysicalTotal:     roundToTwoDecimals(float64(memInfo.Total) / bytesToGB),
		PhysicalAvailable: roundToTwoDecimals(float64(memInfo.Available) / bytesToGB),
		SwapTotal:         roundToTwoDecimals(float64(swapMem.Total) / bytesToGB),
		SwapFree:          roundToTwoDecimals(float64(swapMem.Free) / bytesToGB),
	}, nil
}

func copyFiles(srcFile string, dstFile string) error {
	log.WithFields(log.Fields{
		"src": srcFile,
		"dst": dstFile,
	}).Debug("Copying file")

	sourceFile, err := os.Open(srcFile)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	destinationFile, err := os.Create(dstFile)
	if err != nil {
		return err
	}
	defer destinationFile.Close()

	_, err = io.Copy(destinationFile, sourceFile)
	if err != nil {
		return err
	}

	return nil
}

func WriteOutputToFile(filename string, data []byte) error {
	log.WithField("filename", filename).Debug("Writing output to file")
	err := os.WriteFile(filename, data, 0644)
	if err != nil {
		return err
	}
	return nil
}

func RunCommandsInContainer(ctx context.Context, cli *client.Client, containerID string, commands []NamedCommand) ([]string, error) {
	// Pre-allocate with the number of commands
	filesToWrite := make([]string, 0, len(commands))

	for _, nc := range commands {
		log.WithFields(log.Fields{
			"containerID": containerID,
			"command":     strings.Join(nc.Cmd, " "),
		}).Debug("Running command in container")

		config := container.ExecOptions{
			Cmd:          nc.Cmd,
			Tty:          false,
			AttachStderr: false,
			AttachStdout: true,
			AttachStdin:  false,
			Detach:       true,
		}

		execID, err := cli.ContainerExecCreate(ctx, containerID, config)
		if err != nil {
			log.WithFields(log.Fields{
				"containerID": containerID,
				"command":     strings.Join(nc.Cmd, " "),
				"error":       err,
			}).Error("Error creating exec")
			continue
		}

		resp, err := cli.ContainerExecAttach(ctx, execID.ID, container.ExecStartOptions{})
		if err != nil {
			log.WithFields(log.Fields{
				"containerID": containerID,
				"execID":      execID.ID,
				"error":       err,
			}).Error("Error attaching to exec")
			continue
		}

		output, err := decodeDockerMultiplexedStream(resp.Reader)
		if err != nil {
			log.WithError(err).Error("Error decoding multiplexed stream")
			resp.Close()
			continue
		}

		resp.Close()

		err = WriteOutputToFile(nc.Name, output)
		if err != nil {
			log.WithFields(log.Fields{
				"filename": nc.Name,
				"error":    err,
			}).Error("Error writing output to file")
			continue
		}

		filesToWrite = append(filesToWrite, nc.Name)
	}

	return filesToWrite, nil
}

func decodeDockerMultiplexedStream(reader io.Reader) ([]byte, error) {
	var output []byte
	header := make([]byte, 8)

	for {
		_, err := reader.Read(header)
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}

		size := binary.BigEndian.Uint32(header[4:])
		payload := make([]byte, size)
		_, err = io.ReadFull(reader, payload)

		if err != nil {
			return nil, err
		}

		output = append(output, payload...)
	}

	return output, nil
}

func CopyFilesFromContainers(ctx context.Context, cli *client.Client, containerID string, files []string) ([]string, error) {
	log.WithFields(log.Fields{
		"containerID": containerID,
		"fileCount":   len(files),
	}).Debug("Copying files from container")

	// Pre-allocate with the number of files
	filesToWrite := make([]string, 0, len(files))

	for _, file := range files {
		log.WithFields(log.Fields{
			"containerID": containerID,
			"file":        file,
		}).Debug("Copying file from container")

		reader, _, err := cli.CopyFromContainer(ctx, containerID, file)
		if err != nil {
			log.WithFields(log.Fields{
				"containerID": containerID,
				"file":        file,
				"error":       err,
			}).Error("Error copying file from container")
			continue
		}

		tarReader := tar.NewReader(reader)
		for {
			header, err := tarReader.Next()
			if err == io.EOF {
				break
			}

			if err != nil {
				log.WithError(err).Error("Error reading tar file")
				break
			}

			// Skip non-regular files (directories, symlinks, etc.)
			if header.Typeflag != tar.TypeReg {
				log.WithFields(log.Fields{
					"filename": header.Name,
					"type":     header.Typeflag,
				}).Debug("Skipping non-regular file")
				continue
			}

			// Warn if file is empty
			if header.Size == 0 {
				log.WithFields(log.Fields{
					"filename": header.Name,
					"size":     header.Size,
				}).Warn("File is empty in container")
			}

			outFile, err := os.Create(header.Name)
			if err != nil {
				log.WithFields(log.Fields{
					"filename": header.Name,
					"error":    err,
				}).Error("Error creating file")
				continue
			}

			bytesWritten, err := io.Copy(outFile, tarReader)
			if err != nil {
				log.WithFields(log.Fields{
					"filename": header.Name,
					"error":    err,
				}).Error("Error copying file content")
				outFile.Close()
				continue
			}

			outFile.Close()

			// Only add to list if successfully written
			log.WithFields(log.Fields{
				"filename":     header.Name,
				"bytesWritten": bytesWritten,
			}).Debug("Successfully copied file from container")
			filesToWrite = append(filesToWrite, header.Name)
		}
		reader.Close()
	}

	return filesToWrite, nil
}

func RunCommandInPod(
	ctx context.Context,
	clientset kubernetes.Interface,
	config *rest.Config,
	namespace string,
	pod string,
	container string,
	namedCmd NamedCommand) (string, error) {

	log.WithFields(log.Fields{
		"namespace": namespace,
		"pod":       pod,
		"container": container,
		"command":   strings.Join(namedCmd.Cmd, " "),
	}).Debug("Running command in pod")

	req := clientset.CoreV1().RESTClient().
		Post().
		Resource("pods").
		Name(pod).
		Namespace(namespace).
		SubResource("exec").
		Param("container", container).
		Param("stdin", "false").
		Param("stdout", "true").
		Param("stderr", "true")

	for _, c := range namedCmd.Cmd {
		req.Param("command", c)
	}

	exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		log.WithFields(log.Fields{
			"namespace": namespace,
			"pod":       pod,
			"container": container,
			"error":     err,
		}).Error("Error creating executor")
		return "", err
	}

	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(context.TODO(), remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})

	if err != nil {
		log.WithFields(log.Fields{
			"namespace": namespace,
			"pod":       pod,
			"container": container,
			"error":     err,
			"stderr":    stderr.String(),
		}).Warning("Error streaming command output")
		return "", err
	}

	sanitizedName := strings.ReplaceAll(namedCmd.Name, "/", "-")
	dstFile := fmt.Sprintf("%s-%s.log", container, sanitizedName)

	err = WriteOutputToFile(dstFile, stdout.Bytes())
	if err != nil {
		log.WithFields(log.Fields{
			"filename": dstFile,
			"error":    err,
		}).Error("Error writing file")
		return "", err
	}

	return dstFile, nil
}

func cleanupFiles(filesToCleanup []string) error {
	var failed bool

	for _, file := range filesToCleanup {
		log.WithField("file", file).Debug("Cleaning up file")

		// Check if file exists before attempting removal
		fileInfo, err := os.Stat(file)
		if err != nil {
			if os.IsNotExist(err) {
				log.WithField("file", file).Debug("File already removed or does not exist, skipping")
				continue
			}
			log.WithFields(log.Fields{
				"file":  file,
				"error": err,
			}).Warn("Error checking file status")
			continue
		}

		// Skip if it's a directory
		if fileInfo.IsDir() {
			log.WithField("file", file).Debug("Skipping directory, not a file")
			continue
		}

		// Now remove the file
		err = os.Remove(file)
		if err != nil {
			log.WithFields(log.Fields{
				"file":  file,
				"error": err,
			}).Error("Error removing file")
			failed = true
		}
	}

	if failed {
		return errors.New("Some files could not be removed and may require manual deletion")
	}

	return nil
}

func parseHeaders(headers []string) (http.Header, error) {
	res := http.Header{}
	const splitLen = 2

	for _, keyValue := range headers {
		split := strings.SplitN(keyValue, ":", 2)
		if len(split) >= splitLen {
			res.Add(split[0], split[1])
		} else {
			return nil, fmt.Errorf("splitting header key-value '%s'", keyValue)
		}
	}

	return res, nil
}

type Status struct {
	Database struct {
		Reachable bool `json:"reachable"`
	} `json:"database"`
	Memory struct {
		LuaSharedDicts struct {
			Kong struct {
				AllocatedSlabs string `json:"allocated_slabs"`
				Capacity       string `json:"capacity"`
			} `json:"kong"`
			KongClusterEvents struct {
				AllocatedSlabs string `json:"allocated_slabs"`
				Capacity       string `json:"capacity"`
			} `json:"kong_cluster_events"`
			KongCoreDbCache struct {
				AllocatedSlabs string `json:"allocated_slabs"`
				Capacity       string `json:"capacity"`
			} `json:"kong_core_db_cache"`
			KongCoreDbCacheMiss struct {
				AllocatedSlabs string `json:"allocated_slabs"`
				Capacity       string `json:"capacity"`
			} `json:"kong_core_db_cache_miss"`
			KongCounters struct {
				AllocatedSlabs string `json:"allocated_slabs"`
				Capacity       string `json:"capacity"`
			} `json:"kong_counters"`
			KongDbCache struct {
				AllocatedSlabs string `json:"allocated_slabs"`
				Capacity       string `json:"capacity"`
			} `json:"kong_db_cache"`
			KongDbCacheMiss struct {
				AllocatedSlabs string `json:"allocated_slabs"`
				Capacity       string `json:"capacity"`
			} `json:"kong_db_cache_miss"`
			KongHealthchecks struct {
				AllocatedSlabs string `json:"allocated_slabs"`
				Capacity       string `json:"capacity"`
			} `json:"kong_healthchecks"`
			KongKeyring struct {
				AllocatedSlabs string `json:"allocated_slabs"`
				Capacity       string `json:"capacity"`
			} `json:"kong_keyring"`
			KongLocks struct {
				AllocatedSlabs string `json:"allocated_slabs"`
				Capacity       string `json:"capacity"`
			} `json:"kong_locks"`
			KongProcessEvents struct {
				AllocatedSlabs string `json:"allocated_slabs"`
				Capacity       string `json:"capacity"`
			} `json:"kong_process_events"`
			KongRateLimitingCounters struct {
				AllocatedSlabs string `json:"allocated_slabs"`
				Capacity       string `json:"capacity"`
			} `json:"kong_rate_limiting_counters"`
			KongReportsConsumers struct {
				AllocatedSlabs string `json:"allocated_slabs"`
				Capacity       string `json:"capacity"`
			} `json:"kong_reports_consumers"`
			KongReportsRoutes struct {
				AllocatedSlabs string `json:"allocated_slabs"`
				Capacity       string `json:"capacity"`
			} `json:"kong_reports_routes"`
			KongReportsServices struct {
				AllocatedSlabs string `json:"allocated_slabs"`
				Capacity       string `json:"capacity"`
			} `json:"kong_reports_services"`
			KongReportsWorkspaces struct {
				AllocatedSlabs string `json:"allocated_slabs"`
				Capacity       string `json:"capacity"`
			} `json:"kong_reports_workspaces"`
			KongVitals struct {
				AllocatedSlabs string `json:"allocated_slabs"`
				Capacity       string `json:"capacity"`
			} `json:"kong_vitals"`
			KongVitalsCounters struct {
				AllocatedSlabs string `json:"allocated_slabs"`
				Capacity       string `json:"capacity"`
			} `json:"kong_vitals_counters"`
			KongVitalsLists struct {
				AllocatedSlabs string `json:"allocated_slabs"`
				Capacity       string `json:"capacity"`
			} `json:"kong_vitals_lists"`
			PrometheusMetrics struct {
				AllocatedSlabs string `json:"allocated_slabs"`
				Capacity       string `json:"capacity"`
			} `json:"prometheus_metrics"`
		} `json:"lua_shared_dicts"`
		WorkersLuaVms []struct {
			HTTPAllocatedGc string `json:"http_allocated_gc"`
			Pid             int    `json:"pid"`
		} `json:"workers_lua_vms"`
	} `json:"memory"`
	Server struct {
		ConnectionsAccepted int `json:"connections_accepted"`
		ConnectionsActive   int `json:"connections_active"`
		ConnectionsHandled  int `json:"connections_handled"`
		ConnectionsReading  int `json:"connections_reading"`
		ConnectionsWaiting  int `json:"connections_waiting"`
		ConnectionsWriting  int `json:"connections_writing"`
		TotalRequests       int `json:"total_requests"`
	} `json:"server"`
	ConfigurationHash string `json:"configuration_hash,omitempty" yaml:"configuration_hash,omitempty"`
}

type Workspaces struct {
	Data []struct {
		Comment interface{} `json:"comment"`
		Config  struct {
			Meta                      interface{} `json:"meta"`
			Portal                    bool        `json:"portal"`
			PortalAccessRequestEmail  interface{} `json:"portal_access_request_email"`
			PortalApprovedEmail       interface{} `json:"portal_approved_email"`
			PortalAuth                interface{} `json:"portal_auth"`
			PortalAuthConf            interface{} `json:"portal_auth_conf"`
			PortalAutoApprove         interface{} `json:"portal_auto_approve"`
			PortalCorsOrigins         interface{} `json:"portal_cors_origins"`
			PortalDeveloperMetaFields string      `json:"portal_developer_meta_fields"`
			PortalEmailsFrom          interface{} `json:"portal_emails_from"`
			PortalEmailsReplyTo       interface{} `json:"portal_emails_reply_to"`
			PortalInviteEmail         interface{} `json:"portal_invite_email"`
			PortalIsLegacy            interface{} `json:"portal_is_legacy"`
			PortalResetEmail          interface{} `json:"portal_reset_email"`
			PortalResetSuccessEmail   interface{} `json:"portal_reset_success_email"`
			PortalSessionConf         interface{} `json:"portal_session_conf"`
			PortalTokenExp            interface{} `json:"portal_token_exp"`
		} `json:"config"`
		CreatedAt int    `json:"created_at"`
		ID        string `json:"id"`
		Meta      struct {
			Color     string      `json:"color"`
			Thumbnail interface{} `json:"thumbnail"`
		} `json:"meta"`
		Name string `json:"name"`
	} `json:"data"`
	Next interface{} `json:"next"`
}

type SummaryInfo struct {
	DatabaseType               string `json:"database_type"`
	DeploymentTopology         string `json:"deployment_topology"`
	KongVersion                string `json:"kong_version"`
	TotalConsumerCount         int    `json:"total_consumer_count"`
	TotalDataplaneCount        int    `json:"total_dataplane_count"`
	TotalEnabledDevPortalCount int    `json:"total_enabled_dev_portal_count"`
	TotalPluginCount           int    `json:"total_plugin_count"`
	TotalRouteCount            int    `json:"total_route_count"`
	TotalServiceCount          int    `json:"total_service_count"`
	TotalTargetCount           int    `json:"total_target_count"`
	TotalUpstreamCount         int    `json:"total_upstream_count"`
	TotalWorkspaceCount        int    `json:"total_workspace_count"`
	TotalRegExRoutes           int    `json:"total_regex_route_count"`
}

type CustomMessage struct {
	Message string `json:"message"`
}
