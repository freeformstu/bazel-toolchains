package rbeconfigsgen

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/coreos/go-semver/semver"
)

const (
	buildHeader = `# Copyright 2020 The Bazel Authors. All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#    http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# This file is auto-generated by github.com/bazelbuild/bazel-toolchains/pkg/rbeconfigsgen
# and should not be modified directly.
`
)

var (
	// platformsToolchainBuildTemplate is the template for the BUILD file with the crosstool top
	// toolchain entrypoint target and the default platform definition.
	platformsToolchainBuildTemplate = template.Must(template.New("platformsBuild").Parse(buildHeader + `
package(default_visibility = ["//visibility:public"])

{{ if .CppToolchainTarget }}
toolchain(
    name = "cc-toolchain",
    exec_compatible_with = [
{{ range .ExecConstraints }}        "{{ . }}",
{{ end }}    ],
    target_compatible_with = [
{{ range .TargetConstraints }}        "{{ . }}",
{{ end }}    ],
    toolchain = "{{ .CppToolchainTarget }}",
    toolchain_type = "@bazel_tools//tools/cpp:toolchain_type",
){{ end }}

platform(
    name = "platform",
    parents = ["@local_config_platform//:host"],
    constraint_values = [
{{ range .ExecConstraints }}        "{{ . }}",
{{ end }}    ],
    exec_properties = {
        "container-image": "docker://{{.ToolchainContainer}}",
        "OSFamily": "{{.OSFamily}}",
    },
)
`))
	// legacyJavaBuildTemplate is the Java toolchain config BUILD file template for Bazel versions
	// <4.1.0 (tentative?).
	legacyJavaBuildTemplate = template.Must(template.New("javaBuild").Parse(buildHeader + `
package(default_visibility = ["//visibility:public"])

java_runtime(
    name = "jdk",
    srcs = [],
    java_home = "{{ .JavaHome }}",
)
`))

	// javaBuildTemplate is the Java toolchain config BUILD file template for Bazel versions
	// >=4.1.0 (tentative?).
	javaBuildTemplate = template.Must(template.New("javaBuild").Parse(buildHeader + `
load("@bazel_tools//tools/jdk:local_java_repository.bzl", "local_java_runtime")

package(default_visibility = ["//visibility:public"])

alias(
    name = "jdk",
    actual = "rbe_jdk",
)

local_java_runtime(
    name = "rbe_jdk",
    java_home = "{{ .JavaHome }}",
    version = "{{ .JavaVersion }}",
)
`))

	// imageDigestRegexp is the regex to extract the sha256 digest from a docker image name
	// referenced by its digest.
	imageDigestRegexp = regexp.MustCompile("sha256:([a-f0-9]{64})$")
)

// PlatformToolchainsTemplateParams is used as the input to the toolchains & platform BUILD file
// template 'platformsToolchainBuildTemplate'.
type PlatformToolchainsTemplateParams struct {
	ExecConstraints    []string
	TargetConstraints  []string
	CppToolchainTarget string
	ToolchainContainer string
	OSFamily           string
}

func (p PlatformToolchainsTemplateParams) String() string {
	return fmt.Sprintf("{ExecConstraints: %v, TargetConstraints: %v, CppToolchainTarget: %q, ToolchainContainer: %q, OSFamily: %q}",
		p.ExecConstraints, p.TargetConstraints, p.CppToolchainTarget, p.ToolchainContainer, p.OSFamily)
}

// javaBuildTemplateParams is used as the input to the Java toolchains BUILD file template.
type javaBuildTemplateParams struct {
	JavaHome    string
	JavaVersion string
}

// dockerRunner allows starting a container for a given docker image and subsequently running
// arbitrary commands inside the container or extracting files from it.
// dockerRunner uses the docker client to spin up & interact with containers.
type dockerRunner struct {
	// Input arguments.
	// containerImage is the docker image to spin up as a running container. This could be a tagged
	// or floating reference to a docker image but in a format acceptable to the docker client.
	containerImage string
	// stopContainer determines if the running container will be deleted once we're done with it.
	stopContainer bool

	// Parameters that affect how commands are executed inside the running toolchain container.
	// These parameters can be changed between calls to the execCmd function.

	// workdir is the working directory to use to run commands inside the container.
	workdir string
	// env is the environment variables to set when executing commands specified in the given order
	// as KEY=VALUE strings.
	env []string

	// Populated by the runner.
	// dockerPath is the path to the docker client.
	dockerPath string
	// containerID is the ID of the running docker container.
	containerID string
	// resolvedImage is the container image referenced by its sha256 digest.
	resolvedImage string
}

// generatedFile represents a file part of the toolchain configs generated by the rbeconfigsgen
// package.
type generatedFile struct {
	name     string
	contents []byte
}

// outputConfigs represents input tarballs & files to be assembled into the output toolchain
// configs generated by the rbeconfigsgen package. The generated configs will have the following
// directory structure:
// <configs root>
// |
//  - cc- C++ configs (only if C++ config generation is enabled).
//  - config- C++ crosstool top & default platform definitions.
//  - java- Java toolchain definition.
type outputConfigs struct {
	// cppConfigsTarball is the path to the tarball file containing the C++ configs generated by
	// Bazel inside the toolchain container.
	cppConfigsTarball string
	// configBuild represents the BUILD file containing the C++ crosstool top toolchain target
	// and the default platform definition.
	configBuild generatedFile
	// javaBuild represents the BUILD file containing the java toolchain rule.
	javaBuild generatedFile
}

// runCmd runs an arbitrary command in a shell, logs the exact command that was run and returns
// the generated stdout/stderr. If the command fails, the stdout/stderr is always logged.
func runCmd(cmd string, args ...string) (string, error) {
	cmdStr := fmt.Sprintf("'%s'", strings.Join(append([]string{cmd}, args...), " "))
	log.Printf("Running: %s", cmdStr)
	c := exec.Command(cmd, args...)
	o, err := c.CombinedOutput()
	if err != nil {
		log.Printf("Output: %s", o)
		return "", err
	}
	return string(o), nil
}

// workdir returns the root working directory to use inside the toolchain container for the given
// OS where the OS refers to the OS of the toolchain container.
func workdir(os string) string {
	switch os {
	case OSLinux:
		return "/workdir"
	case OSWindows:
		return "C:/workdir"
	}
	log.Fatalf("Invalid OS: %q", os)
	return ""
}

// bazeliskDownloadInfo returns the URL and name of the local downloaded file to use for downloading
// bazelisk for the given OS.
func bazeliskDownloadInfo(os string) (string, string) {
	switch os {
	case OSLinux:
		return "https://github.com/bazelbuild/bazelisk/releases/download/v1.7.4/bazelisk-linux-amd64", "bazelisk"
	case OSWindows:
		return "https://github.com/bazelbuild/bazelisk/releases/download/v1.7.4/bazelisk-windows-amd64.exe", "bazelisk.exe"
	}
	log.Fatalf("Invalid OS: %q", os)
	return "", ""
}

// newDockerRunner creates a new running container of the given containerImage. stopContainer
// determines if the cleanup function on the dockerRunner will stop the running container when
// called.
func newDockerRunner(containerImage string, stopContainer bool) (*dockerRunner, error) {
	if containerImage == "" {
		return nil, fmt.Errorf("container image was not specified")
	}
	d := &dockerRunner{
		containerImage: containerImage,
		stopContainer:  stopContainer,
		dockerPath:     "docker",
	}
	if _, err := runCmd(d.dockerPath, "pull", d.containerImage); err != nil {
		return nil, fmt.Errorf("docker was unable to pull the toolchain container image %q: %w", d.containerImage, err)
	}
	resolvedImage, err := runCmd(d.dockerPath, "inspect", "--format={{index .RepoDigests 0}}", d.containerImage)
	if err != nil {
		return nil, fmt.Errorf("failed to convert toolchain container image %q into a fully qualified image name by digest: %w", d.containerImage, err)
	}
	resolvedImage = strings.TrimSpace(resolvedImage)
	log.Printf("Resolved toolchain image %q to fully qualified reference %q.", d.containerImage, resolvedImage)
	d.resolvedImage = resolvedImage

	cid, err := runCmd(d.dockerPath, "create", "--rm", d.resolvedImage, "sleep", "infinity")
	if err != nil {
		return nil, fmt.Errorf("failed to create a container with the toolchain container image: %w", err)
	}
	cid = strings.TrimSpace(cid)
	if len(cid) != 64 {
		return nil, fmt.Errorf("container ID %q extracted from the stdout of the container create command had unexpected length, got %d, want 64", cid, len(cid))
	}
	d.containerID = cid
	log.Printf("Created container ID %v for toolchain container image %v.", d.containerID, d.resolvedImage)
	if _, err := runCmd(d.dockerPath, "start", d.containerID); err != nil {
		return nil, fmt.Errorf("failed to run the toolchain container: %w", err)
	}
	return d, nil
}

// execCmd runs the given command inside the docker container and returns the output with whitespace
// trimmed from the edges.
func (d *dockerRunner) execCmd(args ...string) (string, error) {
	a := []string{"exec"}
	if d.workdir != "" {
		a = append(a, "-w", d.workdir)
	}
	for _, e := range d.env {
		a = append(a, "-e", e)
	}
	a = append(a, d.containerID)
	a = append(a, args...)
	o, err := runCmd(d.dockerPath, a...)
	return strings.TrimSpace(o), err
}

// cleanup stops the running container if stopContainer was true when the dockerRunner was created.
func (d *dockerRunner) cleanup() {
	if !d.stopContainer {
		log.Printf("Not stopping container %v of image %v because the Cleanup option was set to false.", d.containerID, d.resolvedImage)
		return
	}
	if _, err := runCmd(d.dockerPath, "stop", "-t", "0", d.containerID); err != nil {
		log.Printf("Failed to stop container %v of toolchain image %v but it's ok to ignore this error if config generation & extraction succeeded.", d.containerID, d.resolvedImage)
	}
}

// copyToContainer copies the local file at 'src' to the container where 'dst' is the path inside
// the container. d.workdir has no impact on this function.
func (d *dockerRunner) copyToContainer(src, dst string) error {
	if _, err := runCmd(d.dockerPath, "cp", src, fmt.Sprintf("%s:%s", d.containerID, dst)); err != nil {
		return err
	}
	return nil
}

// copyFromContainer extracts the file at 'src' from inside the container and copies it to the path
// 'dst' locally. d.workdir has no impact on this function.
func (d *dockerRunner) copyFromContainer(src, dst string) error {
	if _, err := runCmd(d.dockerPath, "cp", fmt.Sprintf("%s:%s", d.containerID, src), dst); err != nil {
		return err
	}
	return nil
}

// getEnv gets the shell environment values from the toolchain container as determined by the
// image config. Env value set or changed by running commands after starting the container aren't
// captured by the return value of this function.
// The return value of this function is a map from env keys to their values. If the image config,
// specifies the same env key multiple times, later values supercede earlier ones.
func (d *dockerRunner) getEnv() (map[string]string, error) {
	result := make(map[string]string)
	o, err := runCmd(d.dockerPath, "inspect", "-f", "{{range $i, $v := .Config.Env}}{{println $v}}{{end}}", d.resolvedImage)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect the docker image to get environment variables: %w", err)
	}
	split := strings.Split(o, "\n")
	for _, s := range split {
		s = strings.TrimSpace(s)
		if len(s) == 0 {
			continue
		}
		keyVal := strings.SplitN(s, "=", 2)
		key := ""
		val := ""
		if len(keyVal) == 2 {
			key, val = keyVal[0], keyVal[1]
		} else if len(keyVal) == 1 {
			// Maybe something like 'KEY=' was specified. We assume value is blank.
			key = keyVal[0]
		}
		if len(key) == 0 {
			continue
		}
		result[key] = val
	}
	return result, nil
}

// installBazelisk downloads bazelisk locally to the specified directory for the given os and copies
// it into the running toolchain container.
// Returns the path Bazelisk was installed to inside the running toolchain container.
func installBazelisk(d *dockerRunner, downloadDir, execOS string) (string, error) {
	url, filename := bazeliskDownloadInfo(execOS)
	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("unable to initiate download for Bazelisk from %s: %w", url, err)
	}
	defer resp.Body.Close()

	localPath := path.Join(downloadDir, filename)
	o, err := os.Create(localPath)
	if err != nil {
		return "", fmt.Errorf("unable to open a file at %q to download Bazelisk to: %w", localPath, err)
	}
	if _, err := io.Copy(o, resp.Body); err != nil {
		return "", fmt.Errorf("error while downloading Bazelisk to %s: %w", localPath, err)
	}

	bazeliskContainerPath := path.Join(d.workdir, filename)
	if err := d.copyToContainer(localPath, bazeliskContainerPath); err != nil {
		return "", fmt.Errorf("failed to copy the downloaded Bazelisk binary into the container: %w", err)
	}

	if _, err := d.execCmd("chmod", "+x", bazeliskContainerPath); err != nil {
		return "", fmt.Errorf("failed to mark the Bazelisk binary as executable inside the container: %w", err)
	}
	return bazeliskContainerPath, nil
}

// appendCppEnv appends environment variables set in the C++ environment map as well as variables
// specified in the C++ environment JSON file to the given environment as "key=value".
func appendCppEnv(env []string, o *Options) ([]string, error) {
	for k, v := range o.CppGenEnv {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}

	if len(o.CppGenEnvJSON) == 0 {
		return env, nil
	}

	blob, err := ioutil.ReadFile(o.CppGenEnvJSON)
	if err != nil {
		return nil, fmt.Errorf("unable to read JSON file %q to read C++ config generation environment variables from: %w", o.CppGenEnvJSON, err)
	}

	e := map[string]string{}
	if err := json.Unmarshal(blob, &e); err != nil {
		return nil, fmt.Errorf("unable to parse file %q as a JSON string -> string dictionary: %w", o.CppGenEnvJSON, err)
	}

	for k, v := range e {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}

	return env, nil
}

// genCppConfigs generates C++ configs inside the running toolchain container represented by the
// given docker runner according to the given options. bazeliskPath is the path to the bazelisk
// binary inside the running toolchain container.
// The return value is the path to the C++ configs tarball copied out of the toolchain container.
func genCppConfigs(d *dockerRunner, o *Options, bazeliskPath string) (string, error) {
	if !o.GenCPPConfigs {
		return "", nil
	}

	// Change the working directory to a dedicated empty directory for C++ configs for each
	// command we run in this function.
	cppProjDir := path.Join(d.workdir, "cpp_configs_project")
	if _, err := d.execCmd("mkdir", cppProjDir); err != nil {
		return "", fmt.Errorf("failed to create empty directory %q inside the toolchain container: %w", cppProjDir, err)
	}
	oldWorkDir := d.workdir
	d.workdir = cppProjDir
	defer func() {
		d.workdir = oldWorkDir
	}()

	if _, err := d.execCmd("touch", "WORKSPACE", "BUILD.bazel"); err != nil {
		return "", fmt.Errorf("failed to create empty build & workspace files in the container to initialize a blank Bazel repository: %w", err)
	}

	// Backup the current environment & restore it before returning.
	oldEnv := d.env
	defer func() {
		d.env = oldEnv
	}()

	// Create a new environment for bazelisk commands used to specify the Bazel version to use to
	// Bazelisk.
	bazeliskEnv := []string{fmt.Sprintf("USE_BAZEL_VERSION=%s", o.BazelVersion)}
	// Add the environment variables needed for the generation only and remove them immediately
	// because they aren't necessary for the config extraction and add unnecessary noise to the
	// logs.
	generationEnv, err := appendCppEnv(bazeliskEnv, o)
	if err != nil {
		return "", fmt.Errorf("failed to add additional environment variables to the C++ config generation docker command: %w", err)
	}
	d.env = generationEnv

	cmd := []string{
		bazeliskPath,
		o.CppBazelCmd,
	}
	cmd = append(cmd, o.CPPConfigTargets...)
	if _, err := d.execCmd(cmd...); err != nil {
		return "", fmt.Errorf("Bazel was unable to build the C++ config generation targets in the toolchain container: %w", err)
	}

	// Restore the env needed for Bazelisk.
	d.env = bazeliskEnv
	bazelOutputRoot, err := d.execCmd(bazeliskPath, "info", "output_base")
	if err != nil {
		return "", fmt.Errorf("unable to determine the build output directory where Bazel produced C++ configs in the toolchain container: %w", err)
	}
	cppConfigDir := path.Join(bazelOutputRoot, "external", o.CPPConfigRepo)
	log.Printf("Extracting C++ config files generated by Bazel at %q from the toolchain container.", cppConfigDir)

	// Restore the old env now that we're done with Bazelisk commands. This is purely to reduce
	// noise in the logs.
	d.env = oldEnv

	// 1. Get a list of symlinks in the config output directory.
	// 2. Harden each link.
	// 3. Archive the contents of the config output directory into a tarball.
	// 4. Copy the tarball from the container to the local temp directory.
	out, err := d.execCmd("find", cppConfigDir, "-type", "l")
	if err != nil {
		return "", fmt.Errorf("unable to list symlinks in the C++ config generation build output directory: %w", err)
	}
	symlinks := strings.Split(out, "\n")
	for _, s := range symlinks {
		resolvedPath, err := d.execCmd("readlink", s)
		if err != nil {
			return "", fmt.Errorf("unable to determine what the symlink %q in %q in the toolchain container points to: %w", s, cppConfigDir, err)
		}
		if _, err := d.execCmd("ln", "-f", resolvedPath, s); err != nil {
			return "", fmt.Errorf("failed to harden symlink %q in %q pointing to %q: %w", s, cppConfigDir, resolvedPath, err)
		}
	}

	outputTarball := "cpp_configs.tar"
	// Explicitly use absolute paths to avoid confusion on what's the working directory.
	outputTarballPath := path.Join(o.TempWorkDir, outputTarball)
	outputTarballContainerPath := path.Join(cppProjDir, outputTarball)
	if _, err := d.execCmd("tar", "-cf", outputTarballContainerPath, "-C", cppConfigDir, "."); err != nil {
		return "", fmt.Errorf("failed to archive the C++ configs into a tarball inside the toolchain container: %w", err)
	}
	if err := d.copyFromContainer(outputTarballContainerPath, outputTarballPath); err != nil {
		return "", fmt.Errorf("failed to copy the C++ config tarball out of the toolchain container: %w", err)
	}
	log.Printf("Generated C++ configs at %s.", outputTarballPath)
	return outputTarballPath, nil
}

// genJavaConfigs returns a BUILD file containing a Java toolchain rule definition that contains
// the following attributes determined by probing details about the JDK version installed in the
// running toolchain container.
// 1. Value of the JAVA_HOME environment variable set in the toolchain image.
// 2. Value of the Java version as reported by the java binary installed in JAVA_HOME inside the
//    running toolchain container.
func genJavaConfigs(d *dockerRunner, o *Options) (generatedFile, error) {
	if !o.GenJavaConfigs {
		return generatedFile{}, nil
	}
	imageEnv, err := d.getEnv()
	if err != nil {
		return generatedFile{}, fmt.Errorf("unable to get the environment of the toolchain image to determine JAVA_HOME: %w", err)
	}
	javaHome, ok := imageEnv["JAVA_HOME"]
	if !ok {
		return generatedFile{}, fmt.Errorf("toolchain image didn't specify environment value JAVA_HOME")
	}
	if len(javaHome) == 0 {
		return generatedFile{}, fmt.Errorf("the value of the JAVA_HOME environment variable was blank in the toolchain image")
	}
	log.Printf("JAVA_HOME was %q.", javaHome)
	javaBin := path.Join(javaHome, "bin/java")
	// "-XshowSettings:properties" is actually what makes java output the version string we're
	// looking for in a more deterministic format. "-version" is just a placeholder so that the
	// command doesn't error out. Although it will likely print the same version string but with
	// some non-deterministic prefix.
	out, err := d.execCmd(javaBin, "-XshowSettings:properties", "-version")
	if err != nil {
		return generatedFile{}, fmt.Errorf("unable to determine the Java version installed in the toolchain container: %w", err)
	}
	javaVersion := ""
	for _, line := range strings.Split(out, "\n") {
		// We're looking for a line that looks like `java.version = <version>` and we want to
		// extract <version>.
		splitVersion := strings.SplitN(line, "=", 2)
		if len(splitVersion) != 2 {
			continue
		}
		key := strings.TrimSpace(splitVersion[0])
		val := strings.TrimSpace(splitVersion[1])
		if key != "java.version" {
			continue
		}
		javaVersion = val
	}
	if len(javaVersion) == 0 {
		return generatedFile{}, fmt.Errorf("unable to determine the java version installed in the container by running 'java -XshowSettings:properties' in the container because it didn't return a line that looked like java.version = <version>")
	}
	log.Printf("Java version: '%s'.", javaVersion)

	bv, err := semver.NewVersion(o.BazelVersion)
	if err != nil {
		return generatedFile{}, fmt.Errorf("unable to parse Bazel version %q as a semver: %w", o.BazelVersion, err)
	}
	t := javaBuildTemplate
	if bv.LessThan(*semver.New("4.1.0")) {
		t = legacyJavaBuildTemplate
	}
	buf := bytes.NewBuffer(nil)
	if err := t.Execute(buf, &javaBuildTemplateParams{
		JavaHome:    javaHome,
		JavaVersion: javaVersion,
	}); err != nil {
		return generatedFile{}, fmt.Errorf("failed to generate the contents of the BUILD file with the Java toolchain definition: %w", err)
	}
	return generatedFile{
		name:     "java/BUILD",
		contents: buf.Bytes(),
	}, nil
}

// processTempDir creates a local temporary working directory to store intermediate files.
func processTempDir(o *Options) error {
	if o.TempWorkDir != "" {
		s, err := os.Stat(o.TempWorkDir)
		if err != nil {
			return fmt.Errorf("got %q specified as option TempWorkDir but the path doesn't exist: %w", o.TempWorkDir, err)
		}
		if !s.IsDir() {
			return fmt.Errorf("got %q specified as option TempWorkDir but the path doesn't point to a directory", o.TempWorkDir)
		}
		return nil
	}
	dir, err := ioutil.TempDir("", "rbeconfigsgen_")
	if err != nil {
		return fmt.Errorf("failed to create a temporary local directory to write intermediate files: %w", err)
	}
	o.TempWorkDir = dir
	return nil
}

// genConfigBuild generates the contents of a BUILD file with a toolchain target pointing to the
// C++ toolchain related rules generated by Bazel and a default platforms target.
func genConfigBuild(o *Options) (generatedFile, error) {
	if o.PlatformParams.CppToolchainTarget != "" {
		return generatedFile{}, fmt.Errorf("<internal error> C++ toolchain target was already set")
	}
	// Populate the C++ toolchain target if C++ config generation is enabled.
	if o.GenCPPConfigs {
		o.PlatformParams.CppToolchainTarget = "//cc:cc-compiler-k8"
		if o.OutputConfigPath != "" {
			o.PlatformParams.CppToolchainTarget = fmt.Sprintf("//%s/cc:cc-compiler-k8", path.Clean(o.OutputConfigPath))
		}
	} else {
		log.Printf("Not generating a toolchain target to be used for the C++ Crosstool top because C++ config generation is disabled.")
	}
	buf := bytes.NewBuffer(nil)
	log.Printf("Fully resolved platform params=%v", o.PlatformParams)
	if err := platformsToolchainBuildTemplate.Execute(buf, o.PlatformParams); err != nil {
		return generatedFile{}, fmt.Errorf("failed to generate platform BUILD file: %w", err)
	}
	return generatedFile{
		name:     "config/BUILD",
		contents: buf.Bytes(),
	}, nil
}

// copyCppConfigsToTarball copies the C++ configs generated by Bazel from the local filesystem at
// 'inTarPath' to the output tarball represented by `outTar`.
func copyCppConfigsToTarball(inTarPath string, outTar *tar.Writer) error {
	in, err := os.Open(inTarPath)
	if err != nil {
		return fmt.Errorf("unable to open input tarball %q for reading: %w", inTarPath, err)
	}
	defer in.Close()
	inTar := tar.NewReader(in)
	pathPrefix := "cc"

	for {
		h, err := inTar.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("error while reading input tarball %q: %w", inTarPath, err)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			break
		case tar.TypeReg:
			if strings.HasSuffix(h.Name, "WORKSPACE") {
				break
			}
			outH := *h
			// Update the name to be in a 'cc' directory and set the mod time to epoch because:
			// 1. The output becomes deterministic.
			// 2. The mod times of the files archived inside the toolchain container sometimes
			//    seem to be well into the future and I didn't bother figuring out why. Maybe it
			//    only happens on my machine (shrug).
			outH.Name = path.Join(pathPrefix, h.Name)
			outH.ModTime = time.Unix(0, 0)
			if err := outTar.WriteHeader(&outH); err != nil {
				return fmt.Errorf("error while adding tar header for %q from input tarball to output tarball: %w", h.Name, err)
			}
			if _, err := io.Copy(outTar, inTar); err != nil {
				return fmt.Errorf("failed to copy the contents of %q from intput tarball to the output tarball: %w", h.Name, err)
			}
		default:
			return fmt.Errorf("got unexpected entry with name %q of type %v in tarball %q: %w", h.Name, h.Typeflag, inTarPath, err)
		}
	}
	return nil
}

// writeGeneratedFileToTarball writes the given generatedFile 'g' to the given output tarball
// 'outTar'.
func writeGeneratedFileToTarball(g generatedFile, outTar *tar.Writer) error {
	if err := outTar.WriteHeader(&tar.Header{
		Name:    g.name,
		Size:    int64(len(g.contents)),
		Mode:    int64(os.ModePerm),
		ModTime: time.Unix(0, 0),
	}); err != nil {
		return fmt.Errorf("failed to write tar header for %q: %w", g.name, err)
	}
	if _, err := io.Copy(outTar, bytes.NewBuffer(g.contents)); err != nil {
		return fmt.Errorf("failed to copy the contents of %q to the output tarball: %w", g.name, err)
	}
	return nil
}

// assembleConfigTarball combines the C++/Java configs represented by 'oc' into a single output
// tarball if requested in the given options.
func assembleConfigTarball(o *Options, oc outputConfigs) error {
	out, err := os.Create(o.OutputTarball)
	if err != nil {
		return fmt.Errorf("unable to open output tarball %q for writing: %w", o.OutputTarball, err)
	}
	outTar := tar.NewWriter(out)

	if o.GenCPPConfigs {
		if err := copyCppConfigsToTarball(oc.cppConfigsTarball, outTar); err != nil {
			return fmt.Errorf("unable to copy C++ configs from the C++ config tarball %q to the output tarball %q: %w", oc.cppConfigsTarball, o.OutputTarball, err)
		}
	}
	if o.GenJavaConfigs {
		if err := writeGeneratedFileToTarball(oc.javaBuild, outTar); err != nil {
			return fmt.Errorf("unable to write the BUILD file %q containing the Java toolchain definition to the output tarball %q: %w", oc.javaBuild.name, o.OutputTarball, err)
		}
	}
	if err := writeGeneratedFileToTarball(oc.configBuild, outTar); err != nil {
		return fmt.Errorf("unable to write the crosstool top/platform BUILD file %q to the output tarball %q: %w", oc.configBuild.name, o.OutputTarball, err)
	}

	// Can't ignore failures when closing the output tarball because it writes metadata without which
	// the tarball is invalid.
	if err := outTar.Close(); err != nil {
		return fmt.Errorf("error trying to finish writing the output tarball %q: %w", o.OutputTarball, err)
	}

	log.Printf("Generated Bazel toolchain configs output tarball %q.", o.OutputTarball)
	return nil
}

// copyCppConfigsToOutputDir extracts the contents of the C++ config tarball at `cppConfigsTarball`
// to the directory at 'outDir'. The C++ config tarball is assumed to contain only regular files,
// i.e., all non-regular files (directories, links, etc) are ignored during the extraction
// process.
func copyCppConfigsToOutputDir(outDir string, cppConfigsTarball string) error {
	in, err := os.Open(cppConfigsTarball)
	if err != nil {
		return fmt.Errorf("unable to open input tarball %q for reading: %w", cppConfigsTarball, err)
	}
	defer in.Close()
	inTar := tar.NewReader(in)

	outDir = path.Join(outDir, "cc")
	for {
		h, err := inTar.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("error while reading input tarball %q: %w", cppConfigsTarball, err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		filePath := path.Join(outDir, h.Name)
		dirPath := path.Dir(filePath)
		if err := os.MkdirAll(dirPath, os.ModePerm); err != nil {
			return fmt.Errorf("unable to create directory %q to extract file %q from the C++ config tarball %q: %w", dirPath, h.Name, cppConfigsTarball, err)
		}
		o, err := os.Create(filePath)
		if err != nil {
			return fmt.Errorf("failed to create file %q for writing %q from the C++ config tarball %q: %w", filePath, h.Name, cppConfigsTarball, err)
		}
		if _, err := io.Copy(o, inTar); err != nil {
			return fmt.Errorf("error while extracting %q from %q to %q: %w", h.Name, cppConfigsTarball, filePath, err)
		}
		o.Close()
	}
	return nil
}

// writeGeneratedFile writes the contents of the file & filename represented by 'g' to the
// given directory.
func writeGeneratedFile(outDir string, g generatedFile) error {
	fullPath := path.Join(outDir, g.name)
	dirPath := path.Dir(fullPath)
	if err := os.MkdirAll(dirPath, os.ModePerm); err != nil {
		return fmt.Errorf("unable to create directory %q to write %q in directory %q: %w", dirPath, g.name, outDir, err)
	}
	if err := ioutil.WriteFile(fullPath, g.contents, os.ModePerm); err != nil {
		return fmt.Errorf("unable to write file %q: %w", fullPath, err)
	}
	return nil
}

// copyConfigsToOutputDir copies the C++/Java configs represented by 'oc' to an output directory
// if one was specified in the given options. This involves extracting C++ configs and generating
// BUILD files for the Java & toolchain entrypoint & platform targets.
func copyConfigsToOutputDir(o *Options, oc outputConfigs) error {
	configsRootDir := path.Join(o.OutputSourceRoot, o.OutputConfigPath)
	if err := os.MkdirAll(configsRootDir, os.ModePerm); err != nil {
		return fmt.Errorf("unable to create directory %q for writing configs: %w", configsRootDir, err)
	}
	if o.GenCPPConfigs {
		if err := copyCppConfigsToOutputDir(configsRootDir, oc.cppConfigsTarball); err != nil {
			return fmt.Errorf("unable to extract C++ configs into output directory %q: %w", configsRootDir, err)
		}
	}
	if o.GenJavaConfigs {
		if err := writeGeneratedFile(configsRootDir, oc.javaBuild); err != nil {
			return fmt.Errorf("unable to write Java configs into output directory %q: %w", configsRootDir, err)
		}
	}
	if err := writeGeneratedFile(configsRootDir, oc.configBuild); err != nil {
		return fmt.Errorf("unable to write the crostool top/platform BUILD file into output directory %q: %w", configsRootDir, err)
	}
	log.Printf("Copied generated configs to directory %q.", configsRootDir)
	return nil
}

// assembleConfigs packages the generated C++/Java configs into a single output as requested by the
// given options. This could involve:
// 1. Generate a single output tarball.
// 2. Copy all configs into a specified directory.
func assembleConfigs(o *Options, oc outputConfigs) error {
	if len(o.OutputTarball) != 0 {
		if err := assembleConfigTarball(o, oc); err != nil {
			return fmt.Errorf("failed to assemble configs into a tarball: %w", err)
		}
	}
	if len(o.OutputSourceRoot) != 0 {
		if err := copyConfigsToOutputDir(o, oc); err != nil {
			return fmt.Errorf("failed to write configs to directory %q: %w", o.OutputSourceRoot, err)
		}
	}
	return nil
}

// digestFile returns the sha256 digest of the contents of the given file.
func digestFile(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("unable to open file %q: %w", filePath, err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("error while hashing the contents of %q: %w", filePath, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// createManifest writes a manifest text file containing information about the generated configs if
// the given options specified a manifest file.
func createManifest(o *Options) error {
	if len(o.OutputManifest) == 0 {
		return nil
	}
	f, err := os.Create(o.OutputManifest)
	if err != nil {
		return fmt.Errorf("unable to open a new file for writing manifest to %q: %w", o.OutputManifest, err)
	}
	defer f.Close()
	fmt.Fprintf(f, "BazelVersion=%s\n", o.BazelVersion)
	fmt.Fprintf(f, "ToolchainContainer=%s\n", o.ToolchainContainer)
	// Extract the sha256 digest from the image name to be included in the manifest.
	s := imageDigestRegexp.FindStringSubmatch(o.PlatformParams.ToolchainContainer)
	if len(s) != 2 {
		return fmt.Errorf("failed to extract sha256 digest using regex from image name %q, got %d substrings, want 2", o.PlatformParams.ToolchainContainer, len(s))
	}
	fmt.Fprintf(f, "ImageDigest=%s\n", s[1])
	fmt.Fprintf(f, "ExecPlatformOS=%s\n", o.PlatformParams.OSFamily)
	// Include the sha256 digest of the configs tarball if output tarball generation was enabled by
	// actually hashing the contents of the output tarball.
	if len(o.OutputTarball) != 0 {
		d, err := digestFile(o.OutputTarball)
		if err != nil {
			return fmt.Errorf("unable to compute the sha256 digest of the output tarball file for the output manifest: %w", err)
		}
		fmt.Fprintf(f, "ConfigsTarballDigest=%s\n", d)
	}
	log.Printf("Wrote output manifest to %q.", o.OutputManifest)
	return nil
}

// Run is the main entrypoint to generate Bazel toolchain configs according to the options
// specified in the given command line arguments.
// The file structure of the generated configs will be as follows:
// <config root>
// |
//  - cc-  C++ configs as generated by Bazel's internal C++ toolchain detection logic.
//  - config- Toolchain entrypoint target for cc_crosstool_top & the auto-generated platform target.
//  - java- Java toolchain definition.
func Run(o Options) error {
	if err := processTempDir(&o); err != nil {
		return fmt.Errorf("unable to initialize a local temporary working directory to store intermediate files: %w", err)
	}
	d, err := newDockerRunner(o.ToolchainContainer, o.Cleanup)
	if err != nil {
		return fmt.Errorf("failed to initialize a docker container: %w", err)
	}
	defer d.cleanup()

	o.PlatformParams.ToolchainContainer = d.resolvedImage

	if _, err := d.execCmd("mkdir", workdir(o.ExecOS)); err != nil {
		return fmt.Errorf("failed to create an empty working directory in the container")
	}
	d.workdir = workdir(o.ExecOS)

	bazeliskPath, err := installBazelisk(d, o.TempWorkDir, o.ExecOS)
	if err != nil {
		return fmt.Errorf("failed to install Bazelisk into the toolchain container: %w", err)
	}

	cppConfigsTarball, err := genCppConfigs(d, &o, bazeliskPath)
	if err != nil {
		return fmt.Errorf("failed to generate C++ configs: %w", err)
	}
	javaBuild, err := genJavaConfigs(d, &o)
	if err != nil {
		return fmt.Errorf("failed to extract information about the installed JDK version in the toolchain container needed to generate Java configs: %w", err)
	}

	configBuild, err := genConfigBuild(&o)
	if err != nil {
		return fmt.Errorf("unable to generate the BUILD file with the C++ crosstool and/or the default platform definition: %w", err)
	}

	oc := outputConfigs{
		cppConfigsTarball: cppConfigsTarball,
		configBuild:       configBuild,
		javaBuild:         javaBuild,
	}
	if err := assembleConfigs(&o, oc); err != nil {
		return fmt.Errorf("unable to assemble C++/Java/Crosstool top/Platform definitions to generate the final toolchain configs output: %w", err)
	}

	if err := createManifest(&o); err != nil {
		return fmt.Errorf("unable to create the manifest file: %w", err)
	}

	if o.Cleanup {
		if err := os.RemoveAll(o.TempWorkDir); err != nil {
			log.Printf("Warning: Unable to delete temporary working directory %q: %v", o.TempWorkDir, err)
		}
	}

	return nil
}