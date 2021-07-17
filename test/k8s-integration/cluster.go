package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	apimachineryversion "k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"
)

func gkeLocationArgs(gceZone, gceRegion string) (locationArg, locationVal string, err error) {
	switch {
	case len(gceZone) > 0:
		locationArg = "--zone"
		locationVal = gceZone
	case len(gceRegion) > 0:
		locationArg = "--region"
		locationVal = gceRegion
	default:
		return "", "", fmt.Errorf("zone and region unspecified")
	}
	return
}

func isRegionalGKECluster(gceZone, gceRegion string) bool {
	return len(gceRegion) > 0
}

func clusterDownGCE(k8sDir string) error {
	cmd := exec.Command(filepath.Join(k8sDir, "hack", "e2e-internal", "e2e-down.sh"))
	err := runCommand("Bringing Down E2E Cluster on GCE", cmd)
	if err != nil {
		return fmt.Errorf("failed to bring down kubernetes e2e cluster on gce: %v", err)
	}
	return nil
}

func clusterDownGKE(gceZone, gceRegion string) error {
	locationArg, locationVal, err := gkeLocationArgs(gceZone, gceRegion)
	if err != nil {
		return err
	}

	cmd := exec.Command("gcloud", "container", "clusters", "delete", *gkeTestClusterName,
		locationArg, locationVal, "--quiet")
	err = runCommand("Bringing Down E2E Cluster on GKE", cmd)
	if err != nil {
		return fmt.Errorf("failed to bring down kubernetes e2e cluster on gke: %v", err)
	}
	return nil
}

func buildKubernetes(k8sDir, command string) error {
	cmd := exec.Command("make", "-C", k8sDir, command)
	err := runCommand("Building Kubernetes", cmd)
	if err != nil {
		return fmt.Errorf("failed to build Kubernetes: %v", err)
	}
	return nil
}

func clusterUpGCE(k8sDir, gceZone string, numNodes int, imageType string) error {
	kshPath := filepath.Join(k8sDir, "cluster", "kubectl.sh")
	_, err := os.Stat(kshPath)
	if err == nil {
		// Set kubectl to the one bundled in the k8s tar for versioning
		err = os.Setenv("GCE_PD_KUBECTL", kshPath)
		if err != nil {
			return fmt.Errorf("failed to set cluster specific kubectl: %v", err)
		}
	} else {
		klog.Errorf("could not find cluster kubectl at %s, falling back to default kubectl", kshPath)
	}

	if len(*kubeFeatureGates) != 0 {
		err = os.Setenv("KUBE_FEATURE_GATES", *kubeFeatureGates)
		if err != nil {
			return fmt.Errorf("failed to set kubernetes feature gates: %v", err)
		}
		klog.V(4).Infof("Set Kubernetes feature gates: %v", *kubeFeatureGates)
	}

	err = setImageTypeEnvs(imageType)
	if err != nil {
		return fmt.Errorf("failed to set image type environment variables: %v", err)
	}

	err = os.Setenv("NUM_NODES", strconv.Itoa(numNodes))
	if err != nil {
		return err
	}

	// The default master size with few nodes is too small; the tests must hit the API server
	// more than usual. The main issue seems to be memory, to reduce GC times that stall the
	// api server. For defaults, get-master-size in k/k/cluster/gce/config-common.sh.
	if numNodes < 20 {
		err = os.Setenv("MASTER_SIZE", "n1-standard-4")
		if err != nil {
			return err
		}
	}

	err = os.Setenv("KUBE_GCE_ZONE", gceZone)
	if err != nil {
		return err
	}
	cmd := exec.Command(filepath.Join(k8sDir, "hack", "e2e-internal", "e2e-up.sh"))
	err = runCommand("Starting E2E Cluster on GCE", cmd)
	if err != nil {
		return fmt.Errorf("failed to bring up kubernetes e2e cluster on gce: %v", err)
	}

	return nil
}

func setImageTypeEnvs(imageType string) error {
	switch strings.ToLower(imageType) {
	case "cos":
	case "gci": // GCI/COS is default type and does not need env vars set
	case "ubuntu":
		return errors.New("setting environment vars for bringing up *ubuntu* cluster on GCE is unimplemented")
		/* TODO(dyzz) figure out how to bring up a Ubuntu cluster on GCE. The below doesn't work.
		err := os.Setenv("KUBE_OS_DISTRIBUTION", "ubuntu")
		if err != nil {
			return err
		}
		err = os.Setenv("KUBE_GCE_NODE_IMAGE", image)
		if err != nil {
			return err
		}
		err = os.Setenv("KUBE_GCE_NODE_PROJECT", imageProject)
		if err != nil {
			return err
		}
		*/
	default:
		return fmt.Errorf("could not set env for image type %s, only gci, cos, ubuntu supported", imageType)
	}
	return nil
}

func clusterUpGKE(gceZone, gceRegion string, numNodes int, imageType string, useManagedDriver bool) error {
	locationArg, locationVal, err := gkeLocationArgs(gceZone, gceRegion)
	if err != nil {
		return err
	}

	out, err := exec.Command("gcloud", "container", "clusters", "list",
		locationArg, locationVal, "--verbosity", "none", "--filter",
		fmt.Sprintf("name=%s", *gkeTestClusterName)).CombinedOutput()

	if err != nil {
		return fmt.Errorf("failed to check for previous test cluster: %v %s", err, out)
	}
	if len(out) > 0 {
		klog.Infof("Detected previous cluster %s. Deleting so a new one can be created...", *gkeTestClusterName)
		err = clusterDownGKE(gceZone, gceRegion)
		if err != nil {
			return err
		}
	}

	var cmd *exec.Cmd
	cmdParams := []string{"container", "clusters", "create", *gkeTestClusterName,
		locationArg, locationVal, "--num-nodes", strconv.Itoa(numNodes),
		"--quiet", "--machine-type", "n1-standard-2", "--image-type", imageType}
	if isVariableSet(gkeClusterVer) {
		cmdParams = append(cmdParams, "--cluster-version", *gkeClusterVer)
	} else {
		cmdParams = append(cmdParams, "--release-channel", *gkeReleaseChannel)
		// release channel based GKE clusters require autorepair to be enabled.
		cmdParams = append(cmdParams, "--enable-autorepair")
	}

	if isVariableSet(gkeNodeVersion) {
		cmdParams = append(cmdParams, "--node-version", *gkeNodeVersion)
	}

	if useManagedDriver {
		cmdParams = append(cmdParams, "--addons", "GcePersistentDiskCsiDriver")
	}

	cmd = exec.Command("gcloud", cmdParams...)
	err = runCommand("Starting E2E Cluster on GKE", cmd)
	if err != nil {
		return fmt.Errorf("failed to bring up kubernetes e2e cluster on gke: %v", err)
	}

	// Because gcloud cannot disable addons on cluster create, the deployment has
	// to be disabled on update.
	clusterVersion := mustGetKubeClusterVersion()
	if !useManagedDriver && isGKEDeploymentInstalledByDefault(clusterVersion) {
		cmd = exec.Command(
			"gcloud", "beta", "container", "clusters", "update",
			*gkeTestClusterName, locationArg, locationVal, "--quiet",
			"--update-addons", "GcePersistentDiskCsiDriver=DISABLED")
		err = runCommand("Updating E2E Cluster on GKE to disable driver deployment", cmd)
		if err != nil {
			return fmt.Errorf("failed to update kubernetes e2e cluster on gke: %v", err)
		}
	}

	return nil
}

func downloadKubernetesSource(pkgDir, k8sIoDir, kubeVersion string) error {
	k8sDir := filepath.Join(k8sIoDir, "kubernetes")
	klog.Infof("Downloading Kubernetes source for %s", kubeVersion)

	if err := os.MkdirAll(k8sIoDir, 0777); err != nil {
		return err
	}
	if err := os.RemoveAll(k8sDir); err != nil {
		return err
	}

	if kubeVersion == "master" {
		// Clone of master. We cannot download the master version from the archive, because the k8s
		// version is not set, which affects which APIs are removed in the running cluster. We cannot
		// use a shallow clone, because in order to find the revision git searches through the tags,
		// and tags are not fetched in a shallow clone. Not using a shallow clone adds about 700M to the
		// ~5G archive directory, after make quick-release, so this is not disastrous.
		klog.Info("cloning k8s master")
		out, err := exec.Command("git", "clone", "https://github.com/kubernetes/kubernetes", k8sDir).CombinedOutput()
		if err != nil {
			return fmt.Errorf("failed to clone kubernetes master: %s, err: %v", out, err)
		}
	} else {
		// Download from the release archives rather than cloning the repo.
		vKubeVersion := "v" + kubeVersion
		kubeTarDir := filepath.Join(k8sIoDir, fmt.Sprintf("kubernetes-%s.tar.gz", kubeVersion))
		klog.Infof("Pulling archive for %s", vKubeVersion)
		out, err := exec.Command("curl", "-L", fmt.Sprintf("https://github.com/kubernetes/kubernetes/archive/%s.tar.gz", vKubeVersion), "-o", kubeTarDir).CombinedOutput()
		if err != nil {
			return fmt.Errorf("failed to curl kubernetes version %s: %s, err: %v", kubeVersion, out, err)
		}

		out, err = exec.Command("tar", "-C", k8sIoDir, "-xvf", kubeTarDir).CombinedOutput()
		if err != nil {
			return fmt.Errorf("failed to untar %s: %s, err: %v", kubeTarDir, out, err)
		}

		err = os.Rename(filepath.Join(k8sIoDir, fmt.Sprintf("kubernetes-%s", kubeVersion)), k8sDir)
		if err != nil {
			return err
		}

		klog.Infof("Successfully downloaded Kubernetes v%s to %s", kubeVersion, k8sDir)
	}
	return nil
}

func getGKEKubeTestArgs(gceZone, gceRegion, imageType string, useKubetest2 bool) ([]string, error) {
	var locationArg, locationVal, locationArgK2 string
	switch {
	case len(gceZone) > 0:
		locationArg = "--gcp-zone"
		locationArgK2 = "--zone"
		locationVal = gceZone
	case len(gceRegion) > 0:
		locationArg = "--gcp-region"
		locationArgK2 = "--region"
		locationVal = gceRegion
	}

	var gkeEnv string
	switch gkeURL := os.Getenv("CLOUDSDK_API_ENDPOINT_OVERRIDES_CONTAINER"); gkeURL {
	case "https://staging-container.sandbox.googleapis.com/":
		gkeEnv = "staging"
	case "https://test-container.sandbox.googleapis.com/":
		gkeEnv = "test"
	case "":
		gkeEnv = "prod"
	default:
		// if the URL does not match to an option, assume it is a custom GKE backend
		// URL and pass that to kubetest
		gkeEnv = gkeURL
	}

	cmd := exec.Command("gcloud", "config", "get-value", "project")
	project, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get current project: %v", err)
	}

	// kubetest arguments
	args := []string{
		"--up=false",
		"--down=false",
		"--provider=gke",
		"--gcp-network=default",
		"--check-version-skew=false",
		"--deployment=gke",
		fmt.Sprintf("--gcp-node-image=%s", imageType),
		"--gcp-network=default",
		fmt.Sprintf("--cluster=%s", *gkeTestClusterName),
		fmt.Sprintf("--gke-environment=%s", gkeEnv),
		fmt.Sprintf("%s=%s", locationArg, locationVal),
		fmt.Sprintf("--gcp-project=%s", project[:len(project)-1]),
	}

	// kubetest2 arguments
	argsK2 := []string{
		"--up=false",
		"--down=false",
		fmt.Sprintf("--cluster-name=%s", *gkeTestClusterName),
		fmt.Sprintf("--environment=%s", gkeEnv),
		fmt.Sprintf("%s=%s", locationArgK2, locationVal),
		fmt.Sprintf("--project=%s", project[:len(project)-1]),
	}

	if useKubetest2 {
		return argsK2, nil
	} else {
		return args, nil
	}
}

func getNormalizedVersion(kubeVersion, gkeVersion string) (string, error) {
	if kubeVersion != "" && gkeVersion != "" {
		return "", fmt.Errorf("both kube version (%s) and gke version (%s) specified", kubeVersion, gkeVersion)
	}
	if kubeVersion == "" && gkeVersion == "" {
		return "", errors.New("neither kube version nor gke version specified")
	}
	var v string
	if kubeVersion != "" {
		v = kubeVersion
	} else if gkeVersion != "" {
		v = gkeVersion
	}
	if v == "master" || v == "latest" {
		// Ugh
		return v, nil
	}
	toks := strings.Split(v, ".")
	if len(toks) < 2 || len(toks) > 3 {
		return "", fmt.Errorf("got unexpected number of tokens in version string %s - wanted 2 or 3", v)
	}
	return strings.Join(toks[:2], "."), nil

}

func getKubeClusterVersion() (string, error) {
	out, err := exec.Command("kubectl", "version", "-o=json").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to obtain cluster version, error: %v", err)
	}
	type version struct {
		ClientVersion *apimachineryversion.Info `json:"clientVersion,omitempty" yaml:"clientVersion,omitempty"`
		ServerVersion *apimachineryversion.Info `json:"serverVersion,omitempty" yaml:"serverVersion,omitempty"`
	}

	var v version
	err = json.Unmarshal(out, &v)
	if err != nil {
		return "", fmt.Errorf("Failed to parse kubectl version output, error: %v", err)
	}

	return v.ServerVersion.GitVersion, nil
}

func mustGetKubeClusterVersion() string {
	ver, err := getKubeClusterVersion()
	if err != nil {
		klog.Fatalf("Error: %v", err)
	}
	return ver
}

// getKubeConfig returns the full path to the
// kubeconfig file set in $KUBECONFIG env.
// If unset, then it defaults to $HOME/.kube/config
func getKubeConfig() (string, error) {
	config, ok := os.LookupEnv("KUBECONFIG")
	if ok {
		return config, nil
	}
	homeDir, ok := os.LookupEnv("HOME")
	if !ok {
		return "", fmt.Errorf("HOME env not set")
	}
	return filepath.Join(homeDir, ".kube/config"), nil
}

// getKubeClient returns a Kubernetes client interface
// for the test cluster
func getKubeClient() (kubernetes.Interface, error) {
	kubeConfig, err := getKubeConfig()
	if err != nil {
		return nil, err
	}
	config, err := clientcmd.BuildConfigFromFlags("", kubeConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create config: %v", err)
	}
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create client: %v", err)
	}
	return kubeClient, nil
}

func isGKEDeploymentInstalledByDefault(clusterVersion string) bool {
	cv := mustParseVersion(clusterVersion)
	return cv.atLeast(mustParseVersion("1.18.10-gke.2101")) &&
		cv.lessThan(mustParseVersion("1.19.0")) ||
		cv.atLeast(mustParseVersion("1.19.3-gke.2100"))
}
