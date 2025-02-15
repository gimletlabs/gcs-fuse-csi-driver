/*
Copyright 2018 The Kubernetes Authors.
Copyright 2022 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	v1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/util/parsers"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const (
	AnnotationGcsfuseVolumeEnableKey                   = "gke-gcsfuse/volumes"
	annotationGcsfuseSidecarCPULimitKey                = "gke-gcsfuse/cpu-limit"
	annotationGcsfuseSidecarMemoryLimitKey             = "gke-gcsfuse/memory-limit"
	annotationGcsfuseSidecarEphemeralStorageLimitKey   = "gke-gcsfuse/ephemeral-storage-limit"
	annotationGcsfuseSidecarCPURequestKey              = "gke-gcsfuse/cpu-request"
	annotationGcsfuseSidecarMemoryRequestKey           = "gke-gcsfuse/memory-request"
	annotationGcsfuseSidecarEphemeralStorageRequestKey = "gke-gcsfuse/ephemeral-storage-request"
)

type SidecarInjector struct {
	Client client.Client
	// default sidecar container config values, can be overwritten by the pod annotations
	Config  *Config
	Decoder *admission.Decoder
}

// Handle injects a gcsfuse sidecar container and a emptyDir to incoming qualified pods.
func (si *SidecarInjector) Handle(_ context.Context, req admission.Request) admission.Response {
	pod := &corev1.Pod{}

	if err := si.Decoder.Decode(req, pod); err != nil {
		klog.Errorf("Could not decode request: name %q, namespace %q, error: %v", req.Name, req.Namespace, err)

		return admission.Errored(http.StatusBadRequest, err)
	}

	if req.Operation != v1.Create {
		return admission.Allowed(fmt.Sprintf("No injection required for operation %v.", req.Operation))
	}

	enableGcsfuseVolumes, ok := pod.Annotations[AnnotationGcsfuseVolumeEnableKey]
	if !ok {
		return admission.Allowed(fmt.Sprintf("The annotation key %q is not found, no injection required.", AnnotationGcsfuseVolumeEnableKey))
	}

	switch strings.ToLower(enableGcsfuseVolumes) {
	case "false":
		return admission.Allowed(fmt.Sprintf("found annotation '%v: false' for Pod: Name %q, GenerateName %q, Namespace %q, no injection required.", AnnotationGcsfuseVolumeEnableKey, pod.Name, pod.GenerateName, pod.Namespace))
	case "true":
		klog.Infof("found annotation '%v: true' for Pod: Name %q, GenerateName %q, Namespace %q, start to inject the sidecar container.", AnnotationGcsfuseVolumeEnableKey, pod.Name, pod.GenerateName, pod.Namespace)
	default:
		return admission.Errored(http.StatusBadRequest, fmt.Errorf("the acceptable values for %q are 'True', 'true', 'false' or 'False'", AnnotationGcsfuseVolumeEnableKey))
	}

	if ValidatePodHasSidecarContainerInjected(pod, true) {
		return admission.Allowed("The sidecar container was injected, no injection required.")
	}

	config, err := si.prepareConfig(pod.Annotations)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	if image, err := parseSidecarContainerImage(pod); err == nil {
		if image != "" {
			config.ContainerImage = image
		}
	} else {
		return admission.Errored(http.StatusBadRequest, err)
	}

	klog.Infof("mutating Pod: Name %q, GenerateName %q, Namespace %q, sidecar image %q, CPU request %q, CPU limit %q, memory request %q, memory limit %q, ephemeral storage request %q, ephemeral storage limit %q",
		pod.Name, pod.GenerateName, pod.Namespace, config.ContainerImage, &config.CPURequest, &config.CPULimit, &config.MemoryRequest, &config.MemoryLimit, &config.EphemeralStorageRequest, &config.EphemeralStorageLimit)
	// the gcsfuse sidecar container has to before the containers that consume the gcsfuse volume
	pod.Spec.Containers = append([]corev1.Container{GetSidecarContainerSpec(config)}, pod.Spec.Containers...)
	pod.Spec.Volumes = append(GetSidecarContainerVolumeSpec(pod.Spec.Volumes), pod.Spec.Volumes...)
	marshaledPod, err := json.Marshal(pod)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, fmt.Errorf("failed to marshal pod: %w", err))
	}

	return admission.PatchResponseFromRaw(req.Object.Raw, marshaledPod)
}

// use the default config values,
// overwritten by the user input from pod annotations.
func (si *SidecarInjector) prepareConfig(annotations map[string]string) (*Config, error) {
	config := &Config{
		ContainerImage:  si.Config.ContainerImage,
		ImagePullPolicy: si.Config.ImagePullPolicy,
	}

	jsonData, err := json.Marshal(annotations)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal pod annotations: %w", err)
	}

	if err := json.Unmarshal(jsonData, config); err != nil {
		return nil, fmt.Errorf("failed to parse sidecar container resource allocation from pod annotations: %w", err)
	}

	// if both of the request and limit are unset, assign the default values.
	// if one of the request or limit is set and another is unset, enforce them to be set as the same.
	populateResource := func(rq, lq *resource.Quantity, drq, dlq resource.Quantity) {
		if rq.Format == "" && lq.Format == "" {
			*rq = drq
			*lq = dlq
		}

		if lq.Format == "" {
			*lq = *rq
		}

		if rq.Format == "" {
			*rq = *lq
		}
	}

	populateResource(&config.CPURequest, &config.CPULimit, si.Config.CPURequest, si.Config.CPULimit)
	populateResource(&config.MemoryRequest, &config.MemoryLimit, si.Config.MemoryRequest, si.Config.MemoryLimit)
	populateResource(&config.EphemeralStorageRequest, &config.EphemeralStorageLimit, si.Config.EphemeralStorageRequest, si.Config.EphemeralStorageLimit)

	return config, nil
}

// iterates the container list,
// if a container is named "gke-gcsfuse-sidecar",
// extract the container image and check if the image is valid,
// then removes this container from the container list.
func parseSidecarContainerImage(pod *corev1.Pod) (string, error) {
	var image string
	var index int
	for i, c := range pod.Spec.Containers {
		if c.Name == SidecarContainerName {
			image = c.Image
			index = i

			if _, _, _, err := parsers.ParseImageName(image); err != nil {
				return "", fmt.Errorf("could not parse input image: %q, error: %w", image, err)
			}
		}
	}

	if image != "" {
		copy(pod.Spec.Containers[index:], pod.Spec.Containers[index+1:])
		pod.Spec.Containers = pod.Spec.Containers[:len(pod.Spec.Containers)-1]
	}

	return image, nil
}
