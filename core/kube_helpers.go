package core

import (
	"strconv"
	"strings"

	"github.com/supergiant/guber"
	"github.com/supergiant/supergiant/types"
)

// kube_helpers.go is a collection of helper methods that convert a Supergiant
// resource definition into a Kubernetes resource defition.
// (and some other assorted things that should maybe be moved out...)

func kubeVolumeMounts(m *types.ContainerBlueprint) (volMounts []*guber.VolumeMount) {
	for _, mount := range m.Mounts {
		volMounts = append(volMounts, asKubeVolumeMount(mount))
	}
	return volMounts
}

func kubeContainerPorts(m *types.ContainerBlueprint) (cPorts []*guber.ContainerPort) {
	for _, port := range m.Ports {
		cPorts = append(cPorts, asKubeContainerPort(port))
	}
	return cPorts
}

func interpolatedEnvVars(m *types.ContainerBlueprint, instance *InstanceResource) (envVars []*guber.EnvVar) {
	for _, envVar := range m.Env {
		envVars = append(envVars, asKubeEnvVar(envVar, instance))
	}
	return envVars
}

func ImageRepoName(m *types.ContainerBlueprint) string {
	return strings.Split(m.Image, "/")[0]
}

func asKubeContainer(m *types.ContainerBlueprint, instance *InstanceResource) *guber.Container { // NOTE how instance must be passed here
	return &guber.Container{
		Name:  "container", // TODO this will fail with multiple containers ------------------------------------ TODO
		Image: m.Image,
		Env:   interpolatedEnvVars(m, instance),
		Resources: &guber.Resources{
			Requests: &guber.ResourceValues{
				Memory: types.BytesFromMiB(m.RAM.Min).ToKubeMebibytes(),
				CPU:    types.CoresFromMillicores(m.CPU.Min).ToKubeMillicores(),
			},
			Limits: &guber.ResourceValues{
				Memory: types.BytesFromMiB(m.RAM.Max).ToKubeMebibytes(),
				CPU:    types.CoresFromMillicores(m.CPU.Max).ToKubeMillicores(),
			},
		},
		VolumeMounts: kubeVolumeMounts(m),
		Ports:        kubeContainerPorts(m),

		// TODO this should be an option, enabled by default with volumes
		SecurityContext: &guber.SecurityContext{
			Privileged: true,
		},
	}
}

// EnvVar
//==============================================================================
func interpolatedValue(m *types.EnvVar, instance *InstanceResource) string {
	r := strings.NewReplacer(
		"{{ instance_id }}", *instance.ID,
		"{{ other_stuff }}", "TODO")
	return r.Replace(m.Value)
}

func asKubeEnvVar(m *types.EnvVar, instance *InstanceResource) *guber.EnvVar {
	return &guber.EnvVar{
		Name:  m.Name,
		Value: interpolatedValue(m, instance),
	}
}

// Volume
//==============================================================================
func asKubeVolume(m *AwsVolume) (*guber.Volume, error) {
	vol, err := m.awsVolume()
	if err != nil {
		return nil, err
	}

	return &guber.Volume{
		Name: *m.Blueprint.Name, // NOTE this is not the physical volume name
		AwsElasticBlockStore: &guber.AwsElasticBlockStore{
			VolumeID: *vol.VolumeId,
			FSType:   "ext4",
		},
	}, nil
}

// Mount
//==============================================================================
func asKubeVolumeMount(m *types.Mount) *guber.VolumeMount {
	return &guber.VolumeMount{
		Name:      *m.Volume,
		MountPath: m.Path,
	}
}

// Port
//==============================================================================
func portName(m *types.Port) string {
	return strconv.Itoa(m.Number)
}

func asKubeContainerPort(m *types.Port) *guber.ContainerPort {
	return &guber.ContainerPort{
		ContainerPort: m.Number,
	}
}

func asKubeServicePort(m *types.Port) *guber.ServicePort {
	return &guber.ServicePort{
		Name:     portName(m),
		Port:     m.Number,
		Protocol: "TCP", // this is default; only other option is UDP
	}
}

// ImageRepo
//==============================================================================
func asKubeImagePullSecret(m *ImageRepoResource) *guber.ImagePullSecret {
	return &guber.ImagePullSecret{
		Name: *m.Name,
	}
}

func asKubeSecret(m *ImageRepoResource) *guber.Secret {
	return &guber.Secret{
		Metadata: &guber.Metadata{
			Name: *m.Name,
		},
		Type: "kubernetes.io/dockercfg",
		Data: map[string]string{
			".dockercfg": m.Key,
		},
	}
}