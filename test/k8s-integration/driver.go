package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"k8s.io/klog"
	"k8s.io/kubernetes/test/e2e/framework/podlogs"
)

func getOverlayDir(pkgDir, deployOverlayName string) string {
	return filepath.Join(pkgDir, "deploy", "kubernetes", "overlays", deployOverlayName)
}

func installDriver(testParams *testParameters, stagingImage, deployOverlayName string, doDriverBuild bool) error {
	if doDriverBuild {
		// Install kustomize
		klog.Infof("Installing kustomize")
		out, err := exec.Command(filepath.Join(testParams.pkgDir, "deploy", "kubernetes", "install-kustomize.sh")).CombinedOutput()
		if err != nil {
			return fmt.Errorf("failed to install kustomize: %s, err: %v", out, err)
		}

		// Edit ci kustomization to use given image tag
		overlayDir := getOverlayDir(testParams.pkgDir, deployOverlayName)
		err = os.Chdir(overlayDir)
		if err != nil {
			return fmt.Errorf("failed to change to overlay directory: %s, err: %v", out, err)
		}

		// TODO (#138): in a local environment this is going to modify the actual kustomize files.
		// maybe a copy should be made instead
		out, err = exec.Command(
			filepath.Join(testParams.pkgDir, "bin", "kustomize"),
			"edit",
			"set",
			"image",
			fmt.Sprintf("%s=%s:%s", pdImagePlaceholder, stagingImage, testParams.stagingVersion)).CombinedOutput()
		if err != nil {
			return fmt.Errorf("failed to edit kustomize: %s, err: %v", out, err)
		}
	}

	var deployEnv []string
	if deployOverlayName != "noauth" {
		// setup service account file for secret creation
		tmpSaFile := filepath.Join(generateUniqueTmpDir(), "cloud-sa.json")
		defer removeDir(filepath.Dir(tmpSaFile))

		// Need to copy it to name the file "cloud-sa.json"
		out, err := exec.Command("cp", *saFile, tmpSaFile).CombinedOutput()
		if err != nil {
			return fmt.Errorf("error copying service account key: %s, err: %v", out, err)
		}
		defer shredFile(tmpSaFile)

		deployEnv = append(deployEnv, fmt.Sprintf("GCE_PD_SA_DIR=%s", filepath.Dir(tmpSaFile)))
	}

	// deploy driver
	deployCmd := exec.Command(filepath.Join(testParams.pkgDir, "deploy", "kubernetes", "deploy-driver.sh"), "--skip-sa-check")
	deployEnv = append(deployEnv,
		fmt.Sprintf("GOPATH=%s", testParams.goPath),
		fmt.Sprintf("GCE_PD_DRIVER_VERSION=%s", deployOverlayName))
	deployEnv = append(os.Environ(), deployEnv...)
	deployCmd.Env = deployEnv
	err := runCommand("Deploying driver", deployCmd)
	if err != nil {
		return fmt.Errorf("failed to deploy driver: %w", err)
	}

	waitScript := filepath.Join(testParams.pkgDir, "deploy", "kubernetes", "wait-for-driver.sh")
	waitCmd := exec.Command(waitScript)
	waitCmd.Env = deployEnv
	err = runCommand("Waiting for driver to start", waitCmd)
	if err != nil {
		return fmt.Errorf("driver failed to come up: %w", err)
	}
	if testParams.platform == "windows" {
		waitCmd = exec.Command(waitScript, "--windows")
		waitCmd.Env = deployEnv
		err = runCommand("Waiting for windows deployment to start", waitCmd)
		if err != nil {
			return fmt.Errorf("Windows deployment failed to come up: %w", err)
		}
	}
	out, err := exec.Command("kubectl", "describe", "pods", "-n", getDriverNamespace()).CombinedOutput()
	klog.Infof("describe pods \n %s", string(out))

	if err != nil {
		return fmt.Errorf("failed to describe pods: %w", err)
	}
	// Print out Windows pod logs for debugging
	if testParams.platform == "windows" {
		podsCmd := exec.Command("kubectl", "get", "pods", "-l", "app=gcp-compute-persistent-disk-csi-driver-win", "-o", "name", "-n", getDriverNamespace())
		out, err = podsCmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("failed to get pd csi pods: %v", err)
		}
		pods := strings.Fields(string(out))
		for _, pod := range pods {
			logCmd := exec.Command("kubectl", "logs", pod, "-n", getDriverNamespace(), "--all-containers=true")
			out, err := logCmd.CombinedOutput()
			if err != nil {
				return fmt.Errorf("failed to get logs from pod %s: %v", pod, err)
			}
			klog.Infof("kubectl logs pod: %s, output %s", pod, string(out))
		}
	}

	return nil
}

func deleteDriver(testParams *testParameters, deployOverlayName string) error {
	deleteCmd := exec.Command(filepath.Join(testParams.pkgDir, "deploy", "kubernetes", "delete-driver.sh"))
	deleteCmd.Env = append(os.Environ(),
		fmt.Sprintf("GOPATH=%s", testParams.goPath),
		fmt.Sprintf("GCE_PD_DRIVER_VERSION=%s", deployOverlayName),
	)
	err := runCommand("Deleting driver", deleteCmd)
	if err != nil {
		return fmt.Errorf("failed to delete driver: %v", err)
	}
	return nil
}

func pushImage(pkgDir, stagingImage, stagingVersion, platform string) error {
	err := os.Setenv("GCE_PD_CSI_STAGING_VERSION", stagingVersion)
	if err != nil {
		return err
	}
	err = os.Setenv("GCE_PD_CSI_STAGING_IMAGE", stagingImage)
	if err != nil {
		return err
	}
	var cmd *exec.Cmd

	if platform == "windows" {
		// build multi-arch image which can work for both Linux and Windows
		cmd = exec.Command("make", "-C", pkgDir, "build-and-push-multi-arch",
			fmt.Sprintf("GCE_PD_CSI_STAGING_VERSION=%s", stagingVersion),
			fmt.Sprintf("GCE_PD_CSI_STAGING_IMAGE=%s", stagingImage))
		err = runCommand("Building and Pushing GCP Container for Windows", cmd)
		if err != nil {
			return fmt.Errorf("failed to run make command for windows: err: %v", err)
		}
	} else {
		cmd = exec.Command("make", "-C", pkgDir, "build-and-push-container-linux",
			fmt.Sprintf("GCE_PD_CSI_STAGING_VERSION=%s", stagingVersion),
			fmt.Sprintf("GCE_PD_CSI_STAGING_IMAGE=%s", stagingImage))
		err = runCommand("Pushing GCP Container for Linux", cmd)
		if err != nil {
			return fmt.Errorf("failed to run make command for linux: err: %v", err)
		}
	}

	return nil
}

func deleteImage(stagingImage, stagingVersion string) error {
	cmd := exec.Command("gcloud", "container", "images", "delete", fmt.Sprintf("%s:%s", stagingImage, stagingVersion), "--quiet")
	err := runCommand("Deleting GCR Container", cmd)
	if err != nil {
		return fmt.Errorf("failed to delete container image %s:%s: %s", stagingImage, stagingVersion, err)
	}
	return nil
}

// dumpDriverLogs will watch all pods in the driver namespace
// and copy its logs to the test artifacts directory, if set.
// It returns a context.CancelFunc that needs to be invoked when
// the test is finished.
func dumpDriverLogs() (context.CancelFunc, error) {
	// Dump all driver logs to the test artifacts
	artifactsDir, ok := os.LookupEnv("ARTIFACTS")
	if ok {
		client, err := getKubeClient()
		if err != nil {
			return nil, fmt.Errorf("failed to get kubeclient: %v", err)
		}
		out := podlogs.LogOutput{
			StatusWriter:  os.Stdout,
			LogPathPrefix: filepath.Join(artifactsDir, "pd-csi-driver") + "/",
		}
		ctx, cancel := context.WithCancel(context.Background())
		if err = podlogs.CopyAllLogs(ctx, client, getDriverNamespace(), out); err != nil {
			return cancel, fmt.Errorf("failed to start pod logger: %v", err)
		}
		return cancel, nil
	}
	return nil, nil
}

// mergeArtifacts merges the results of doing multiple gingko runs, taking all junit files
// in the specified subdirectories of the artifacts directory and merging into a single
// file at the artifcats root.  If artifacts are not saved (ie, ARTIFACTS is not set),
// this is a no-op. See kubernetes-csi/csi-release-tools/prow.sh for the inspiration.
func mergeArtifacts(subdirectories []string) error {
	artifactsDir, ok := os.LookupEnv("ARTIFACTS")
	if !ok {
		// No artifacts, nothing to merge.
		return nil
	}
	var sourceDirs []string
	for _, subdir := range subdirectories {
		sourceDirs = append(sourceDirs, filepath.Join(artifactsDir, subdir))
	}
	return MergeJUnit("External Storage", sourceDirs, filepath.Join(artifactsDir, "junit_pdcsi.xml"))
}

func getDriverNamespace() string {
	if *useGKEManagedDriver {
		return managedDriverNamespace
	}
	return externalDriverNamespace
}
