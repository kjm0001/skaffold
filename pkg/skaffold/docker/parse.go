/*
Copyright 2018 Google LLC

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

package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/schema/v1alpha2"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/util"
	"github.com/google/go-containerregistry/v1"

	"github.com/docker/docker/builder/dockerignore"
	"github.com/docker/docker/pkg/fileutils"
	"github.com/moby/moby/builder/dockerfile/parser"
	"github.com/moby/moby/builder/dockerfile/shell"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const (
	add    = "add"
	copy   = "copy"
	env    = "env"
	from   = "from"
	expose = "expose"
)

// For testing.
var RetrieveImage = retrieveImage

type DockerfileDepResolver struct{}

func (d *DockerfileDepResolver) GetDependencies(a *v1alpha2.Artifact) ([]string, error) {
	return GetDockerfileDependencies(a.DockerArtifact.DockerfilePath, a.Workspace)
}

// GetDockerfileDependencies parses a dockerfile and returns the full paths
// of all the source files that the resulting docker image depends on.
func GetDockerfileDependencies(dockerfilePath, workspace string) ([]string, error) {
	path := filepath.Join(workspace, dockerfilePath)
	f, err := util.Fs.Open(path)
	if err != nil {
		return nil, errors.Wrapf(err, "opening dockerfile: %s", path)
	}
	defer f.Close()

	res, err := parser.Parse(f)
	if err != nil {
		return nil, errors.Wrap(err, "parsing dockerfile")
	}

	envs := map[string]string{}
	depMap := map[string]struct{}{}
	// First process onbuilds, if present.
	onbuildsImages := [][]string{}
	for _, value := range res.AST.Children {
		switch value.Value {
		case from:
			onbuilds, err := processBaseImage(value)
			if err != nil {
				logrus.Warnf("Error processing base image for onbuild triggers: %s. Dependencies may be incomplete.", err)
			}
			onbuildsImages = append(onbuildsImages, onbuilds)
		}
	}

	var dispatchInstructions = func(r *parser.Result) {
		for _, value := range r.AST.Children {
			switch value.Value {
			case add, copy:
				processCopy(workspace, value, depMap, envs)
			case env:
				envs[value.Next.Value] = value.Next.Next.Value
			}
		}
	}
	for _, image := range onbuildsImages {
		for _, ob := range image {
			obRes, err := parser.Parse(strings.NewReader(ob))
			if err != nil {
				return nil, err
			}
			dispatchInstructions(obRes)
		}
	}

	dispatchInstructions(res)

	deps := []string{}
	for dep := range depMap {
		deps = append(deps, dep)
	}
	logrus.Infof("Found dependencies for dockerfile %s", deps)

	expandedDeps, err := util.ExpandPaths(workspace, deps)
	if err != nil {
		return nil, errors.Wrap(err, "expanding dockerfile paths")
	}
	logrus.Infof("deps %s", expandedDeps)

	if !util.StrSliceContains(expandedDeps, path) {
		expandedDeps = append(expandedDeps, path)
	}

	// Look for .dockerignore.
	ignorePath := filepath.Join(workspace, ".dockerignore")
	filteredDeps, err := ApplyDockerIgnore(expandedDeps, ignorePath)
	if err != nil {
		return nil, errors.Wrap(err, "applying dockerignore")
	}

	return filteredDeps, nil
}

func PortsFromDockerfile(r io.Reader) ([]string, error) {
	res, err := parser.Parse(r)
	if err != nil {
		return nil, errors.Wrap(err, "parsing dockerfile")
	}

	// Check the dockerfile and the base.
	ports := []string{}
	for _, value := range res.AST.Children {
		switch value.Value {
		case from:
			base := value.Next.Value
			if strings.ToLower(base) == "scratch" {
				logrus.Debug("Skipping port check in SCRATCH base image.")
				continue
			}
			img, err := RetrieveImage(value.Next.Value)
			if err != nil {
				logrus.Warnf("Error checking base image for ports: %s", err)
				continue
			}
			for port := range img.Config.ExposedPorts {
				logrus.Debugf("Found port %s in base image", port)
				ports = append(ports, port)
			}
		case expose:
			// There can be multiple ports per line.
			for {
				if value.Next == nil {
					break
				}
				port := value.Next.Value
				logrus.Debugf("Found port %s in Dockerfile", port)
				ports = append(ports, port)
				value = value.Next
			}
		}
	}
	// Sort ports for consistency in tests.
	sort.Strings(ports)
	return ports, nil
}

func processBaseImage(value *parser.Node) ([]string, error) {
	base := value.Next.Value
	logrus.Debugf("Checking base image %s for ONBUILD triggers.", base)
	if strings.ToLower(base) == "scratch" {
		logrus.Debugf("SCRATCH base image found, skipping check: %s", base)
		return nil, nil
	}
	img, err := RetrieveImage(base)
	if err != nil {
		return nil, err
	}
	logrus.Debugf("Found onbuild triggers %v in image %s", img.Config.OnBuild, base)
	return img.Config.OnBuild, nil
}

var imageCache sync.Map

func retrieveImage(image string) (*v1.ConfigFile, error) {
	cachedCfg, present := imageCache.Load(image)
	if present {
		return cachedCfg.(*v1.ConfigFile), nil
	}

	client, err := NewDockerAPIClient()
	if err != nil {
		return nil, err
	}

	cfg := &v1.ConfigFile{}
	raw, err := retrieveLocalImage(client, image)
	if err == nil {
		if err := json.Unmarshal(raw, cfg); err != nil {
			return nil, err
		}
	} else {
		cfg, err = retrieveRemoteConfig(image)
		if err != nil {
			return nil, errors.Wrap(err, "getting remote config")
		}
	}

	imageCache.Store(image, cfg)

	return cfg, nil
}

func retrieveLocalImage(client DockerAPIClient, image string) ([]byte, error) {
	_, raw, err := client.ImageInspectWithRaw(context.Background(), image)
	if err != nil {
		return nil, err
	}

	return raw, nil
}

func retrieveRemoteConfig(identifier string) (*v1.ConfigFile, error) {
	img, err := remoteImage(identifier)
	if err != nil {
		return nil, errors.Wrap(err, "getting image")
	}

	return img.ConfigFile()
}

func processCopy(workspace string, value *parser.Node, paths map[string]struct{}, envs map[string]string) error {
	slex := shell.NewLex('\\')
	for {
		// Skip last node, since it is the destination, and stop if we arrive at a comment
		if value.Next.Next == nil || strings.HasPrefix(value.Next.Next.Value, "#") {
			break
		}
		src, err := processShellWord(slex, value.Next.Value, envs)
		if err != nil {
			return errors.Wrap(err, "processing word")
		}
		// If the --from flag is provided, we are dealing with a multi-stage dockerfile
		// Adding a dependency from a different stage does not imply a source dependency
		if hasMultiStageFlag(value.Flags) {
			return nil
		}
		if !strings.HasPrefix(src, "http://") && !strings.HasPrefix(src, "https://") {
			dep := path.Join(workspace, src)
			paths[dep] = struct{}{}
		} else {
			logrus.Debugf("Skipping watch on remote dependency %s", src)
		}

		value = value.Next
	}
	return nil
}

func processShellWord(lex *shell.Lex, word string, envs map[string]string) (string, error) {
	envSlice := []string{}
	for envKey, envVal := range envs {
		envSlice = append(envSlice, fmt.Sprintf("%s=%s", envKey, envVal))
	}
	return lex.ProcessWord(word, envSlice)
}

func hasMultiStageFlag(flags []string) bool {
	for _, f := range flags {
		if strings.HasPrefix(f, "--from=") {
			return true
		}
	}
	return false
}

func ApplyDockerIgnore(paths []string, dockerIgnorePath string) ([]string, error) {
	absPaths, err := util.RelPathToAbsPath(paths)
	if err != nil {
		return nil, errors.Wrap(err, "getting absolute path of dependencies")
	}
	excludes := []string{}
	if _, err := util.Fs.Stat(dockerIgnorePath); !os.IsNotExist(err) {
		r, err := util.Fs.Open(dockerIgnorePath)
		if err != nil {
			return nil, err
		}
		defer r.Close()

		excludes, err = dockerignore.ReadAll(r)
		if err != nil {
			return nil, err
		}
		excludes = append(excludes, ".dockerignore")
	}

	absPathExcludes, err := util.RelPathToAbsPath(excludes)
	if err != nil {
		return nil, errors.Wrap(err, "getting absolute path of docker ignored paths")
	}

	filteredDeps := []string{}
	for _, d := range absPaths {
		m, err := fileutils.Matches(d, absPathExcludes)
		if err != nil {
			return nil, err
		}
		if !m {
			filteredDeps = append(filteredDeps, d)
		}
	}
	sort.Strings(filteredDeps)
	return filteredDeps, nil
}
