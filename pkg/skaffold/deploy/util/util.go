/*
Copyright 2019 The Skaffold Authors

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

package util

import (
	"fmt"
	"io/ioutil"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	k8s "k8s.io/client-go/kubernetes"
	fakekubeclientset "k8s.io/client-go/kubernetes/fake"

	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/config"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/deploy/types"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/docker"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/graph"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/instrumentation"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/kubernetes"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/kubernetes/manifest"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/schema/latest"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/util/stringset"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/yaml"
)

// ApplyDefaultRepo applies the default repo to a given image tag.
func ApplyDefaultRepo(globalConfig string, defaultRepo *string, tag string) (string, error) {
	repo, err := config.GetDefaultRepo(globalConfig, defaultRepo)
	if err != nil {
		return "", fmt.Errorf("getting default repo: %w", err)
	}

	multiLevel, err := config.GetMultiLevelRepo(globalConfig)
	if err != nil {
		return "", fmt.Errorf("getting multi-level repo support: %w", err)
	}

	newTag, err := docker.SubstituteDefaultRepoIntoImage(repo, multiLevel, tag)
	if err != nil {
		return "", fmt.Errorf("applying default repo to %q: %w", tag, err)
	}

	return newTag, nil
}

// Update which images are logged, if the image is present in the provided deployer's artifacts.
func AddTagsToPodSelector(artifacts []graph.Artifact, deployerArtifacts []graph.Artifact, podSelector *kubernetes.ImageList) {
	m := map[string]bool{}
	for _, a := range deployerArtifacts {
		m[a.ImageName] = true
	}
	for _, artifact := range artifacts {
		imageName := docker.SanitizeImageName(artifact.ImageName)
		if _, ok := m[imageName]; ok {
			podSelector.Add(artifact.Tag)
		}
	}
}

func MockK8sClient(string) (k8s.Interface, error) {
	return fakekubeclientset.NewSimpleClientset(), nil
}

func ConsolidateNamespaces(original, new []string) []string {
	if len(new) == 0 {
		return original
	}
	namespaces := stringset.New()
	namespaces.Insert(append(original, new...)...)
	namespaces.Delete("") // if we have provided namespaces, remove the empty "default" namespace
	return namespaces.ToList()
}

// GroupVersionResource returns the first `GroupVersionResource` for the given `GroupVersionKind`.
func GroupVersionResource(disco discovery.DiscoveryInterface, gvk schema.GroupVersionKind) (bool, schema.GroupVersionResource, error) {
	resources, err := disco.ServerResourcesForGroupVersion(gvk.GroupVersion().String())
	if err != nil {
		return false, schema.GroupVersionResource{}, fmt.Errorf("getting server resources for group version: %w", err)
	}

	for _, r := range resources.APIResources {
		if r.Kind == gvk.Kind {
			return r.Namespaced, schema.GroupVersionResource{
				Group:    gvk.Group,
				Version:  gvk.Version,
				Resource: r.Name,
			}, nil
		}
	}

	return false, schema.GroupVersionResource{}, fmt.Errorf("could not find resource for %s", gvk.String())
}

func ConsolidateTransformConfiguration(cfg types.Config) (map[schema.GroupKind]latest.ResourceFilter, map[schema.GroupKind]latest.ResourceFilter, error) {
	// TODO(aaron-prindle) currently this also modifies the flag & config to support a JSON path syntax for input.
	// this should be done elsewhere eventually

	transformableAllowlist := map[schema.GroupKind]latest.ResourceFilter{}
	transformableDenylist := map[schema.GroupKind]latest.ResourceFilter{}
	// add default values
	for _, rf := range manifest.TransformAllowlist {
		groupKind := schema.ParseGroupKind(rf.GroupKind)
		transformableAllowlist[groupKind] = convertJSONPathIndex(rf)
	}
	for _, rf := range manifest.TransformDenylist {
		groupKind := schema.ParseGroupKind(rf.GroupKind)
		transformableDenylist[groupKind] = convertJSONPathIndex(rf)
	}

	// add user schema values, override defaults
	for _, rf := range cfg.TransformAllowList() {
		instrumentation.AddResourceFilter("schema", "allow")
		groupKind := schema.ParseGroupKind(rf.GroupKind)
		transformableAllowlist[groupKind] = convertJSONPathIndex(rf)
		delete(transformableDenylist, groupKind)
	}
	for _, rf := range cfg.TransformDenyList() {
		instrumentation.AddResourceFilter("schema", "deny")
		groupKind := schema.ParseGroupKind(rf.GroupKind)
		transformableDenylist[groupKind] = convertJSONPathIndex(rf)
		delete(transformableAllowlist, groupKind)
	}

	// add user flag values, override user schema values and defaults
	// TODO(aaron-prindle) see if workdir needs to be considered in this read
	if cfg.TransformRulesFile() != "" {
		transformRulesFromFile, err := ioutil.ReadFile(cfg.TransformRulesFile())
		if err != nil {
			return nil, nil, err
		}
		rsc := latest.ResourceSelectorConfig{}
		err = yaml.Unmarshal(transformRulesFromFile, &rsc)
		if err != nil {
			return nil, nil, err
		}
		for _, rf := range rsc.Allow {
			instrumentation.AddResourceFilter("cli-flag", "allow")
			groupKind := schema.ParseGroupKind(rf.GroupKind)
			transformableAllowlist[groupKind] = convertJSONPathIndex(rf)
			delete(transformableDenylist, groupKind)
		}

		for _, rf := range rsc.Deny {
			instrumentation.AddResourceFilter("cli-flag", "deny")
			groupKind := schema.ParseGroupKind(rf.GroupKind)
			transformableDenylist[groupKind] = convertJSONPathIndex(rf)
			delete(transformableAllowlist, groupKind)
		}
	}

	return transformableAllowlist, transformableDenylist, nil
}

func convertJSONPathIndex(rf latest.ResourceFilter) latest.ResourceFilter {
	nrf := latest.ResourceFilter{}
	nrf.GroupKind = rf.GroupKind

	if len(rf.Labels) > 0 {
		nlabels := []string{}
		for _, str := range rf.Labels {
			if str == ".*" {
				nlabels = append(nlabels, str)
				continue
			}
			nstr := strings.ReplaceAll(str, ".*", "")
			nlabels = append(nlabels, nstr)
		}
		nrf.Labels = nlabels
	}

	if len(rf.Image) > 0 {
		nimage := []string{}
		for _, str := range rf.Image {
			if str == ".*" {
				nimage = append(nimage, str)
				continue
			}
			nstr := strings.ReplaceAll(str, ".*", "")
			nimage = append(nimage, nstr)
		}
		nrf.Image = nimage
	}

	return nrf
}
