// Copyright 2017 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package provider

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/juju/errors"
	"github.com/juju/loggo"
	"github.com/juju/retry"
	"github.com/juju/utils/clock"
	"gopkg.in/juju/names.v2"
	core "k8s.io/api/core/v1"
	"k8s.io/api/extensions/v1beta1"
	storage "k8s.io/api/storage/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/juju/juju/agent"
	"github.com/juju/juju/caas"
	"github.com/juju/juju/core/application"
	"github.com/juju/juju/environs"
	"github.com/juju/juju/environs/context"
	"github.com/juju/juju/juju/paths"
	"github.com/juju/juju/network"
	"github.com/juju/juju/status"
	"github.com/juju/juju/watcher"
)

var logger = loggo.GetLogger("juju.kubernetes.provider")

const (
	labelOperator    = "juju-operator"
	labelStorage     = "juju-storage"
	labelVersion     = "juju-version"
	labelApplication = "juju-application"
	labelUnit        = "juju-unit"

	// Well known storage pool attributes.
	jujuStorageClassKey = "juju-storage-class"
	jujuStorageLabelKey = "juju-storage-label"

	operatorStorageClassName = "juju-operator-storage"
	// TODO(caas) - make this configurable using application config
	operatorStorageSize = "10Mi"
)

var defaultPropagationPolicy = v1.DeletePropagationForeground

type kubernetesClient struct {
	kubernetes.Interface

	// namespace is the k8s namespace to use when
	// creating k8s resources.
	namespace string
}

// To regenerate the mocks for the kubernetes Client used by this broker,
// run "go generate" from the package directory.
//go:generate mockgen -package mocks -destination mocks/k8sclient_mock.go k8s.io/client-go/kubernetes Interface
//go:generate mockgen -package mocks -destination mocks/corev1_mock.go k8s.io/client-go/kubernetes/typed/core/v1 CoreV1Interface,NamespaceInterface,PodInterface,ServiceInterface,ConfigMapInterface,PersistentVolumeInterface,PersistentVolumeClaimInterface
//go:generate mockgen -package mocks -destination mocks/extenstionsv1_mock.go k8s.io/client-go/kubernetes/typed/extensions/v1beta1 ExtensionsV1beta1Interface,DeploymentInterface,IngressInterface
//go:generate mockgen -package mocks -destination mocks/storagev1_mock.go k8s.io/client-go/kubernetes/typed/storage/v1 StorageV1Interface,StorageClassInterface

// NewK8sClientFunc defines a function which returns a k8s client based on the supplied config.
type NewK8sClientFunc func(c *rest.Config) (kubernetes.Interface, error)

// NewK8sBroker returns a kubernetes client for the specified k8s cluster.
func NewK8sBroker(cloudSpec environs.CloudSpec, namespace string, newClient NewK8sClientFunc) (caas.Broker, error) {
	config, err := newK8sConfig(cloudSpec)
	if err != nil {
		return nil, errors.Trace(err)
	}
	client, err := newClient(config)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return &kubernetesClient{Interface: client, namespace: namespace}, nil
}

func newK8sConfig(cloudSpec environs.CloudSpec) (*rest.Config, error) {
	if cloudSpec.Credential == nil {
		return nil, errors.Errorf("cloud %v has no credential", cloudSpec.Name)
	}

	var CAData []byte
	for _, cacert := range cloudSpec.CACertificates {
		CAData = append(CAData, cacert...)
	}

	credentialAttrs := cloudSpec.Credential.Attributes()
	return &rest.Config{
		Host:     cloudSpec.Endpoint,
		Username: credentialAttrs[CredAttrUsername],
		Password: credentialAttrs[CredAttrPassword],
		TLSClientConfig: rest.TLSClientConfig{
			CertData: []byte(credentialAttrs["ClientCertificateData"]),
			KeyData:  []byte(credentialAttrs["ClientKeyData"]),
			CAData:   CAData,
		},
	}, nil
}

// Provider is part of the Broker interface.
func (*kubernetesClient) Provider() caas.ContainerEnvironProvider {
	return providerInstance
}

// Destroy is part of the Broker interface.
func (k *kubernetesClient) Destroy(context.ProviderCallContext) error {
	return k.deleteNamespace()
}

// EnsureNamespace ensures this broker's namespace is created.
func (k *kubernetesClient) EnsureNamespace() error {
	ns := &core.Namespace{ObjectMeta: v1.ObjectMeta{Name: k.namespace}}
	namespaces := k.CoreV1().Namespaces()
	_, err := namespaces.Update(ns)
	if k8serrors.IsNotFound(err) {
		_, err = namespaces.Create(ns)
	}
	return errors.Trace(err)
}

func (k *kubernetesClient) deleteNamespace() error {
	// deleteNamespace is used as a means to implement Destroy().
	// All model resources are provisioned in the namespace;
	// deleting the namespace will also delete those resources.
	namespaces := k.CoreV1().Namespaces()
	err := namespaces.Delete(k.namespace, &v1.DeleteOptions{
		PropagationPolicy: &defaultPropagationPolicy,
	})
	if k8serrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return errors.Trace(err)
	}
	return errors.Trace(err)
}

// EnsureOperator creates or updates an operator pod with the given application
// name, agent path, and operator config.
func (k *kubernetesClient) EnsureOperator(appName, agentPath string, config *caas.OperatorConfig) error {
	logger.Debugf("creating/updating %s operator", appName)

	// TODO(caas) - this is a stop gap until we implement a CAAS model manager worker
	// First up, ensure the namespace eis there if not already created.
	if err := k.EnsureNamespace(); err != nil {
		return errors.Annotatef(err, "ensuring operator namespace %v", k.namespace)
	}

	// TODO(caas) use secrets for storing agent password?
	if err := k.ensureConfigMap(operatorConfigMap(appName, config)); err != nil {
		return errors.Annotate(err, "creating or updating ConfigMap")
	}

	// Attempt to get a persistent volume to store charm state etc.
	// If there are none, that's ok, we'll just use ephemeral storage.
	volName := fmt.Sprintf("%s-operator-volume", appName)
	volStorageLabel := fmt.Sprintf("%s-operator-storage", appName)
	params := volumeParams{
		storageClassName: operatorStorageClassName,
		storageLabels:    []string{volStorageLabel, k.namespace, "default"},
		pvcName:          operatorVolumeClaim(appName),
		volumeName:       volName,
		volumeSize:       operatorStorageSize,
		labels:           map[string]string{labelApplication: appName},
	}
	storageVol, err := k.maybeGetVolume(params)
	if err != nil && !errors.IsNotFound(err) {
		return errors.Annotate(err, "finding operator volume")
	}
	pod := operatorPod(appName, agentPath, config.OperatorImagePath, config.Version.String())
	if storageVol != nil {
		logger.Debugf("using persistent volume for operator: %+v", storageVol)
		pod.Spec.Volumes = append(pod.Spec.Volumes, *storageVol)
		pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, core.VolumeMount{
			Name:      storageVol.Name,
			MountPath: agent.BaseDir(agentPath),
		})
	}

	// See if we are able to just update the pod image, otherwise we'll need to
	// delete and create as without deployment controller that's all we can do.
	// TODO(caas) - consider using a deployment controller for operator for easier management
	if err := k.maybeUpdatePodImage(
		operatorSelector(appName), config.Version.String(), pod.Spec.Containers[0].Image); err != nil {
		return k.ensurePod(pod)
	}
	return nil
}

// maybeGetStorageClass looks for a storage class to use when creating
// a persistent volume, using the specified name (if supplied), or a class
// matching the specified labels.
func (k *kubernetesClient) maybeGetStorageClass(name string, labels ...string) (*storage.StorageClass, error) {
	// First try looking for a storage class by name.
	if name != "" {
		sc, err := k.StorageV1().StorageClasses().Get(name, v1.GetOptions{IncludeUninitialized: true})
		if err == nil {
			return sc, nil
		}
		if !k8serrors.IsNotFound(err) {
			return nil, errors.Trace(err)
		}
	}

	// Second try looking for a storage class with a Juju label.
	selector := fmt.Sprintf("%v in (%v)", labelStorage, strings.Join(labels, ", "))
	storageClasses, err := k.StorageV1().StorageClasses().List(v1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil, errors.Trace(err)
	}
	logger.Debugf("available storage classes: %v", storageClasses.Items)
	// For now, pick the first matching storage class.
	if len(storageClasses.Items) > 0 {
		return &storageClasses.Items[0], nil
	}

	// Third look for the cluster default storage class, if defined.
	storageClasses, err = k.StorageV1().StorageClasses().List(v1.ListOptions{})
	if err != nil {
		return nil, errors.Trace(err)
	}
	for _, sc := range storageClasses.Items {
		if v, ok := sc.Annotations["storageclass.kubernetes.io/is-default-class"]; ok && v != "false" {
			logger.Debugf("using default storage class: %v", sc.Name)
			return &sc, nil
		}
	}
	return nil, errors.NotFoundf("storage class for any %q", labels)
}

func operatorVolumeClaim(appName string) string {
	return fmt.Sprintf("%v-operator-volume-claim", appName)
}

// getAvailableVolume looks for an available volume with any of
// the specified labels. If storageClassName is specified, the
// volume must also match that as well.
func (k *kubernetesClient) getAvailableVolume(storageClassName string, labels ...string) (string, string, error) {
	selector := fmt.Sprintf("%v in (%v)", labelStorage, strings.Join(labels, ", "))
	pvs, err := k.CoreV1().PersistentVolumes().List(v1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return "", "", errors.Trace(err)
	}

	for _, pv := range pvs.Items {
		if pv.Status.Phase != core.VolumeAvailable {
			logger.Debugf("ignoring volume %v for %v, status is %v", pv.Name, selector, pv.Status.Phase)
			continue
		}
		if storageClassName != "" && pv.Spec.StorageClassName != storageClassName {
			continue
		}
		logger.Debugf("using existing volume: %v", pv.Name)
		return pv.Name, pv.Spec.StorageClassName, nil
	}
	return "", "", errors.NotFoundf("persistent volume for any %q", labels)
}

type volumeParams struct {
	storageLabels    []string
	storageClassName string
	pvcName          string
	volumeName       string
	volumeSize       string
	labels           map[string]string
	accessMode       core.PersistentVolumeAccessMode
}

// maybeGetVolume attempts to create a persistent volume.
func (k *kubernetesClient) maybeGetVolume(params volumeParams) (*core.Volume, error) {
	makeVolumeSpec := func(volName string) *core.Volume {
		vol := &core.Volume{Name: volName}
		vol.PersistentVolumeClaim = &core.PersistentVolumeClaimVolumeSource{
			ClaimName: params.pvcName,
		}
		return vol
	}

	// We create a volume using a persistent volume claim.
	// First, attempt to get any previously created claim for this app.
	pvClaims := k.CoreV1().PersistentVolumeClaims(k.namespace)
	pvc, err := pvClaims.Get(params.pvcName, v1.GetOptions{})
	if err != nil && !k8serrors.IsNotFound(err) {
		return nil, errors.Trace(err)
	}
	if err == nil {
		logger.Debugf("using existing pvc %s for %v", pvc.Name, pvc.Spec.VolumeName)
		return makeVolumeSpec(pvc.Spec.VolumeName), nil
	}

	// We need to create a new claim.
	logger.Debugf("creating new persistent volume claim for %v", params.volumeName)

	// First, see if the user has set up a labelled persistent volume for this app or model.
	pvName, scName, err := k.getAvailableVolume(params.storageClassName, params.storageLabels...)
	if err != nil && !errors.IsNotFound(err) {
		return nil, errors.Trace(err)
	}

	if errors.IsNotFound(err) {
		// No existing persistent volumes have been set up, so attempt to create
		// a new one using a storage class.
		sc, err := k.maybeGetStorageClass(params.storageClassName, params.storageLabels...)
		if errors.IsNotFound(err) {
			return nil, errors.NewNotFound(nil, fmt.Sprintf(
				"cannot create persistent volume as no storage class matching %q exists and no default storage class is defined",
				params.storageLabels))
		}
		if err != nil {
			return nil, errors.Trace(err)
		}
		scName = sc.Name
	}
	accessMode := params.accessMode
	if accessMode == "" {
		accessMode = core.ReadWriteOnce
	}
	fsSize, err := resource.ParseQuantity(params.volumeSize)
	if err != nil {
		return nil, errors.Trace(err)
	}
	pvc, err = pvClaims.Create(&core.PersistentVolumeClaim{
		ObjectMeta: v1.ObjectMeta{
			Name:   params.pvcName,
			Labels: params.labels},
		Spec: core.PersistentVolumeClaimSpec{
			VolumeName:       pvName,
			StorageClassName: &scName,
			Resources: core.ResourceRequirements{
				Requests: core.ResourceList{
					core.ResourceStorage: fsSize,
				},
			},
			AccessModes: []core.PersistentVolumeAccessMode{accessMode},
		},
	})
	if err != nil {
		return nil, errors.Trace(err)
	}
	logger.Debugf("created new pvc: %+v", pvc)

	return makeVolumeSpec(pvc.Spec.VolumeName), nil
}

// DeleteOperator deletes the specified operator.
func (k *kubernetesClient) DeleteOperator(appName string) (err error) {
	logger.Debugf("deleting %s operator", appName)

	// First delete any persistent volume claim.
	pvClaims := k.CoreV1().PersistentVolumeClaims(k.namespace)
	pvcName := operatorVolumeClaim(appName)
	err = pvClaims.Delete(pvcName, &v1.DeleteOptions{
		PropagationPolicy: &defaultPropagationPolicy,
	})
	if err != nil && !k8serrors.IsNotFound(err) {
		return nil
	}

	// Then delete the config map.
	configMaps := k.CoreV1().ConfigMaps(k.namespace)
	configMapName := operatorConfigMapName(appName)
	err = configMaps.Delete(configMapName, &v1.DeleteOptions{
		PropagationPolicy: &defaultPropagationPolicy,
	})
	if err != nil && !k8serrors.IsNotFound(err) {
		return nil
	}

	// Finally the pod itself.
	podName := operatorPodName(appName)
	return k.deletePod(podName)
}

// Service returns the service for the specified application.
func (k *kubernetesClient) Service(appName string) (*caas.Service, error) {
	services := k.CoreV1().Services(k.namespace)
	servicesList, err := services.List(v1.ListOptions{
		LabelSelector: applicationSelector(appName),
	})
	if err != nil {
		return nil, errors.Trace(err)
	}
	if len(servicesList.Items) == 0 {
		return nil, errors.NotFoundf("service for %q", appName)
	}
	service := servicesList.Items[0]
	result := caas.Service{
		Id: string(service.UID),
	}
	if service.Spec.ClusterIP != "" {
		result.Addresses = append(result.Addresses, network.Address{
			Value: service.Spec.ClusterIP,
			Type:  network.DeriveAddressType(service.Spec.ClusterIP),
			Scope: network.ScopeCloudLocal,
		})
	}
	if service.Spec.LoadBalancerIP != "" {
		result.Addresses = append(result.Addresses, network.Address{
			Value: service.Spec.LoadBalancerIP,
			Type:  network.DeriveAddressType(service.Spec.LoadBalancerIP),
			Scope: network.ScopePublic,
		})
	}
	for _, addr := range service.Spec.ExternalIPs {
		result.Addresses = append(result.Addresses, network.Address{
			Value: addr,
			Type:  network.DeriveAddressType(addr),
			Scope: network.ScopePublic,
		})
	}
	return &result, nil
}

// DeleteService deletes the specified service.
func (k *kubernetesClient) DeleteService(appName string) (err error) {
	logger.Debugf("deleting application %s", appName)

	if err := k.deleteService(appName); err != nil {
		return errors.Trace(err)
	}
	return errors.Trace(k.deleteDeployment(appName))
}

// EnsureService creates or updates a service for pods with the given params.
func (k *kubernetesClient) EnsureService(
	appName string, params *caas.ServiceParams, numUnits int, config application.ConfigAttributes,
) (err error) {
	logger.Debugf("creating/updating application %s", appName)

	if numUnits <= 0 {
		return errors.Errorf("number of units must be > 0")
	}
	if params == nil || params.PodSpec == nil {
		return errors.Errorf("missing pod spec")
	}

	var cleanups []func()
	defer func() {
		if err == nil {
			return
		}
		for _, f := range cleanups {
			f()
		}
	}()

	unitSpec, err := makeUnitSpec(params.PodSpec)
	if err != nil {
		return errors.Annotatef(err, "parsing unit spec for %s", appName)
	}

	// Now add in any storage requested.
	if err := k.configureStorage(unitSpec, appName, params); err != nil {
		return errors.Annotatef(err, "configuring storage for %s", appName)
	}

	// Add a deployment controller configured to create the specified number of units/pods.
	numPods := int32(numUnits)
	if err := k.configureDeployment(appName, unitSpec, params.PodSpec.Containers, &numPods); err != nil {
		return errors.Annotate(err, "creating or updating deployment controller")
	}
	cleanups = append(cleanups, func() { k.deleteDeployment(appName) })

	var ports []core.ContainerPort
	for _, c := range unitSpec.Pod.Containers {
		for _, p := range c.Ports {
			if p.ContainerPort == 0 {
				continue
			}
			ports = append(ports, p)
		}
	}
	if !params.PodSpec.OmitServiceFrontend {
		if err := k.configureService(appName, ports, config); err != nil {
			return errors.Annotatef(err, "creating or updating service for %v", appName)
		}
	}
	return nil
}

func (k *kubernetesClient) configureStorage(unitSpec *unitSpec, appName string, params *caas.ServiceParams) error {
	baseDir, err := paths.StorageDir("kubernetes")
	if err != nil {
		return errors.Trace(err)
	}
	for i, fs := range params.Filesystems {
		var mountPath string
		if fs.Attachment != nil {
			mountPath = fs.Attachment.Path
		}
		if mountPath == "" {
			mountPath = fmt.Sprintf("%s/fs/%s/%s", baseDir, appName, fs.Tag.Id())
		}
		volName := fmt.Sprintf("%s-fsvolume-%d", appName, i)
		volStorageLabel := fmt.Sprintf("%s-unit-storage", appName)
		params := volumeParams{
			storageLabels: []string{volStorageLabel, k.namespace, "default"},
			pvcName:       volName + "-claim",
			volumeName:    volName,
			volumeSize:    fmt.Sprintf("%dMi", fs.Size),
			labels:        map[string]string{labelApplication: appName},
		}
		if storageClassName, ok := fs.Attributes[jujuStorageClassKey]; ok {
			params.storageClassName = fmt.Sprintf("%v", storageClassName)
		}
		if storageLabel, ok := fs.Attributes[jujuStorageLabelKey]; ok {
			params.storageLabels = append([]string{fmt.Sprintf("%v", storageLabel)}, params.storageLabels...)
		}

		storageVol, err := k.maybeGetVolume(params)
		if err != nil {
			return errors.Annotatef(err, "finding volume for %s", fs.Tag.String())
		}
		logger.Debugf("using persistent volume %q for %s filesystem %s: %+v", storageVol.Name, appName, fs.Tag.Id(), storageVol)
		unitSpec.Pod.Volumes = append(unitSpec.Pod.Volumes, *storageVol)
		unitSpec.Pod.Containers[0].VolumeMounts = append(unitSpec.Pod.Containers[0].VolumeMounts, core.VolumeMount{
			Name:      storageVol.Name,
			MountPath: mountPath,
		})
	}
	return nil
}

type configMapNameFunc func(fileSetName string) string

func (k *kubernetesClient) configurePodFiles(podSpec *core.PodSpec, containers []caas.ContainerSpec, cfgMapName configMapNameFunc) error {
	for i, container := range containers {
		for _, fileSet := range container.Files {
			cfgName := cfgMapName(fileSet.Name)
			vol := core.Volume{Name: cfgName}
			if err := k.ensureConfigMap(filesetConfigMap(cfgName, &fileSet)); err != nil {
				return errors.Annotatef(err, "creating or updating ConfigMap for file set %v", cfgName)
			}
			vol.ConfigMap = &core.ConfigMapVolumeSource{
				LocalObjectReference: core.LocalObjectReference{
					Name: cfgName,
				},
			}
			podSpec.Volumes = append(podSpec.Volumes, vol)
			podSpec.Containers[i].VolumeMounts = append(podSpec.Containers[i].VolumeMounts, core.VolumeMount{
				Name:      cfgName,
				MountPath: fileSet.MountPath,
			})
		}
	}
	return nil
}

func (k *kubernetesClient) configureDeployment(appName string, unitSpec *unitSpec, containers []caas.ContainerSpec, replicas *int32) error {
	logger.Debugf("creating/updating deployment for %s", appName)

	// Add the specified file to the pod spec.
	cfgName := func(fileSetName string) string {
		return applicationConfigMapName(appName, fileSetName)
	}
	podSpec := unitSpec.Pod
	if err := k.configurePodFiles(&podSpec, containers, cfgName); err != nil {
		return errors.Trace(err)
	}

	namePrefix := resourceNamePrefix(appName)
	deployment := &v1beta1.Deployment{
		ObjectMeta: v1.ObjectMeta{
			Name:   deploymentName(appName),
			Labels: map[string]string{labelApplication: appName}},
		Spec: v1beta1.DeploymentSpec{
			Replicas: replicas,
			Selector: &v1.LabelSelector{
				MatchLabels: map[string]string{labelApplication: appName},
			},
			Template: core.PodTemplateSpec{
				ObjectMeta: v1.ObjectMeta{
					GenerateName: namePrefix,
					Labels:       map[string]string{labelApplication: appName},
				},
				Spec: podSpec,
			},
		},
	}
	return k.ensureDeployment(deployment)
}

func (k *kubernetesClient) ensureDeployment(spec *v1beta1.Deployment) error {
	deployments := k.ExtensionsV1beta1().Deployments(k.namespace)
	_, err := deployments.Update(spec)
	if k8serrors.IsNotFound(err) {
		_, err = deployments.Create(spec)
	}
	return errors.Trace(err)
}

func (k *kubernetesClient) deleteDeployment(appName string) error {
	deployments := k.ExtensionsV1beta1().Deployments(k.namespace)
	err := deployments.Delete(deploymentName(appName), &v1.DeleteOptions{
		PropagationPolicy: &defaultPropagationPolicy,
	})
	if k8serrors.IsNotFound(err) {
		return nil
	}
	return errors.Trace(err)
}

func (k *kubernetesClient) configureService(appName string, containerPorts []core.ContainerPort, config application.ConfigAttributes) error {
	logger.Debugf("creating/updating service for %s", appName)

	var ports []core.ServicePort
	for i, cp := range containerPorts {
		// We normally expect a single container port for most use cases.
		// We allow the user to specify what first service port should be,
		// otherwise it just defaults to the container port.
		// TODO(caas) - consider allowing all service ports to be specified
		var targetPort intstr.IntOrString
		if i == 0 {
			targetPort = intstr.FromInt(config.GetInt(serviceTargetPortConfigKey, int(cp.ContainerPort)))
		}
		ports = append(ports, core.ServicePort{
			Protocol:   cp.Protocol,
			Port:       cp.ContainerPort,
			TargetPort: targetPort,
		})
	}

	serviceType := core.ServiceType(config.GetString(serviceTypeConfigKey, defaultServiceType))
	service := &core.Service{
		ObjectMeta: v1.ObjectMeta{
			Name:   deploymentName(appName),
			Labels: map[string]string{labelApplication: appName}},
		Spec: core.ServiceSpec{
			Selector:                 map[string]string{labelApplication: appName},
			Type:                     serviceType,
			Ports:                    ports,
			ExternalIPs:              config.Get(serviceExternalIPsConfigKey, []string(nil)).([]string),
			LoadBalancerIP:           config.GetString(serviceLoadBalancerIPKey, ""),
			LoadBalancerSourceRanges: config.Get(serviceLoadBalancerSourceRangesKey, []string(nil)).([]string),
			ExternalName:             config.GetString(serviceExternalNameKey, ""),
		},
	}
	return k.ensureService(service)
}

func (k *kubernetesClient) ensureService(spec *core.Service) error {
	services := k.CoreV1().Services(k.namespace)
	// Set any immutable fields if the service already exists.
	existing, err := services.Get(spec.Name, v1.GetOptions{IncludeUninitialized: true})
	if err == nil {
		spec.Spec.ClusterIP = existing.Spec.ClusterIP
		spec.ObjectMeta.ResourceVersion = existing.ObjectMeta.ResourceVersion
	}
	_, err = services.Update(spec)
	if k8serrors.IsNotFound(err) {
		_, err = services.Create(spec)
	}
	return errors.Trace(err)
}

func (k *kubernetesClient) deleteService(appName string) error {
	services := k.CoreV1().Services(k.namespace)
	err := services.Delete(deploymentName(appName), &v1.DeleteOptions{
		PropagationPolicy: &defaultPropagationPolicy,
	})
	if k8serrors.IsNotFound(err) {
		return nil
	}
	return errors.Trace(err)
}

// ExposeService sets up external access to the specified application.
func (k *kubernetesClient) ExposeService(appName string, config application.ConfigAttributes) error {
	logger.Debugf("creating/updating ingress resource for %s", appName)

	host := config.GetString(caas.JujuExternalHostNameKey, "")
	if host == "" {
		return errors.Errorf("external hostname required")
	}
	ingressClass := config.GetString(ingressClassKey, defaultIngressClass)
	ingressSSLRedirect := config.GetBool(ingressSSLRedirectKey, defaultIngressSSLRedirect)
	ingressSSLPassthrough := config.GetBool(ingressSSLPassthroughKey, defaultIngressSSLPassthrough)
	ingressAllowHTTP := config.GetBool(ingressAllowHTTPKey, defaultIngressAllowHTTPKey)
	httpPath := config.GetString(caas.JujuApplicationPath, caas.JujuDefaultApplicationPath)
	if httpPath == "$appname" {
		httpPath = appName
	}
	if !strings.HasPrefix(httpPath, "/") {
		httpPath = "/" + httpPath
	}

	svc, err := k.CoreV1().Services(k.namespace).Get(deploymentName(appName), v1.GetOptions{})
	if err != nil {
		return errors.Trace(err)
	}
	if len(svc.Spec.Ports) == 0 {
		return errors.Errorf("cannot create ingress rule for service %q without a port", svc.Name)
	}
	spec := &v1beta1.Ingress{
		ObjectMeta: v1.ObjectMeta{
			Name:   deploymentName(appName),
			Labels: map[string]string{labelApplication: appName},
			Annotations: map[string]string{
				"ingress.kubernetes.io/rewrite-target":  "",
				"ingress.kubernetes.io/ssl-redirect":    strconv.FormatBool(ingressSSLRedirect),
				"kubernetes.io/ingress.class":           ingressClass,
				"kubernetes.io/ingress.allow-http":      strconv.FormatBool(ingressAllowHTTP),
				"ingress.kubernetes.io/ssl-passthrough": strconv.FormatBool(ingressSSLPassthrough),
			},
		},
		Spec: v1beta1.IngressSpec{
			Rules: []v1beta1.IngressRule{{
				Host: host,
				IngressRuleValue: v1beta1.IngressRuleValue{
					HTTP: &v1beta1.HTTPIngressRuleValue{
						Paths: []v1beta1.HTTPIngressPath{{
							Path: httpPath,
							Backend: v1beta1.IngressBackend{
								ServiceName: svc.Name, ServicePort: svc.Spec.Ports[0].TargetPort},
						}}},
				}}},
		},
	}
	return k.ensureIngress(spec)
}

// UnexposeService removes external access to the specified service.
func (k *kubernetesClient) UnexposeService(appName string) error {
	logger.Debugf("deleting ingress resource for %s", appName)
	return k.deleteIngress(appName)
}

func (k *kubernetesClient) ensureIngress(spec *v1beta1.Ingress) error {
	ingress := k.ExtensionsV1beta1().Ingresses(k.namespace)
	_, err := ingress.Update(spec)
	if k8serrors.IsNotFound(err) {
		_, err = ingress.Create(spec)
	}
	return errors.Trace(err)
}

func (k *kubernetesClient) deleteIngress(appName string) error {
	ingress := k.ExtensionsV1beta1().Ingresses(k.namespace)
	err := ingress.Delete(deploymentName(appName), &v1.DeleteOptions{
		PropagationPolicy: &defaultPropagationPolicy,
	})
	if k8serrors.IsNotFound(err) {
		return nil
	}
	return errors.Trace(err)
}

func operatorSelector(appName string) string {
	return fmt.Sprintf("%v==%v", labelOperator, appName)
}

func applicationSelector(appName string) string {
	return fmt.Sprintf("%v==%v", labelApplication, appName)
}

// WatchUnits returns a watcher which notifies when there
// are changes to units of the specified application.
func (k *kubernetesClient) WatchUnits(appName string) (watcher.NotifyWatcher, error) {
	pods := k.CoreV1().Pods(k.namespace)
	w, err := pods.Watch(v1.ListOptions{
		LabelSelector: applicationSelector(appName),
		Watch:         true,
	})
	if err != nil {
		return nil, errors.Trace(err)
	}
	return newKubernetesWatcher(w, appName)
}

// Units returns all units of the specified application.
func (k *kubernetesClient) Units(appName string) ([]caas.Unit, error) {
	pods := k.CoreV1().Pods(k.namespace)
	podsList, err := pods.List(v1.ListOptions{
		LabelSelector: applicationSelector(appName),
	})
	if err != nil {
		return nil, errors.Trace(err)
	}
	var result []caas.Unit
	now := time.Now()
	for _, p := range podsList.Items {
		var ports []string
		for _, c := range p.Spec.Containers {
			for _, p := range c.Ports {
				ports = append(ports, fmt.Sprintf("%v/%v", p.ContainerPort, p.Protocol))
			}
		}
		terminated := p.DeletionTimestamp != nil
		unitInfo := caas.Unit{
			Id:      string(p.UID),
			Address: p.Status.PodIP,
			Ports:   ports,
			Dying:   terminated,
			Status: status.StatusInfo{
				Status:  k.jujuStatus(p.Status.Phase, terminated),
				Message: p.Status.Message,
				Since:   &now,
			},
		}
		// If the pod is a Juju unit label, it was created directly
		// by Juju an we can extract the unit tag to include on the result.
		unitLabel := p.Labels[labelUnit]
		if strings.HasPrefix(unitLabel, "juju-") {
			unitTag, err := names.ParseUnitTag(unitLabel[5:])
			if err == nil {
				unitInfo.UnitTag = unitTag.String()
			}
		}
		result = append(result, unitInfo)
	}
	return result, nil
}

func (k *kubernetesClient) jujuStatus(podPhase core.PodPhase, terminated bool) status.Status {
	if terminated {
		return status.Terminated
	}
	switch podPhase {
	case core.PodRunning:
		return status.Running
	case core.PodFailed:
		return status.Error
	case core.PodPending:
		return status.Allocating
	default:
		return status.Unknown
	}
}

// EnsureUnit creates or updates a unit pod with the given unit name and spec.
func (k *kubernetesClient) EnsureUnit(appName, unitName string, spec *caas.PodSpec) error {
	logger.Debugf("creating/updating unit %s", unitName)
	unitSpec, err := makeUnitSpec(spec)
	if err != nil {
		return errors.Annotatef(err, "parsing spec for %s", unitName)
	}
	podName := unitPodName(unitName)
	pod := core.Pod{
		ObjectMeta: v1.ObjectMeta{
			Name: podName,
			Labels: map[string]string{
				labelApplication: appName,
				labelUnit:        podName}},
		Spec: unitSpec.Pod,
	}

	// Add the specified file to the pod spec.
	cfgName := func(fileSetName string) string {
		return unitConfigMapName(unitName, fileSetName)
	}
	if err := k.configurePodFiles(&pod.Spec, spec.Containers, cfgName); err != nil {
		return errors.Trace(err)
	}
	return k.ensurePod(&pod)
}

// filesetConfigMap returns a *core.ConfigMap for a pod
// of the specified unit, with the specified files.
func filesetConfigMap(configMapName string, files *caas.FileSet) *core.ConfigMap {
	result := &core.ConfigMap{
		ObjectMeta: v1.ObjectMeta{
			Name: configMapName,
		},
		Data: map[string]string{},
	}
	for name, data := range files.Files {
		result.Data[name] = data
	}
	return result
}

// DeleteUnit deletes a unit pod with the given unit name.
func (k *kubernetesClient) DeleteUnit(unitName string) error {
	logger.Debugf("deleting unit %s", unitName)
	podName := unitPodName(unitName)
	return k.deletePod(podName)
}

func (k *kubernetesClient) ensureConfigMap(configMap *core.ConfigMap) error {
	configMaps := k.CoreV1().ConfigMaps(k.namespace)
	_, err := configMaps.Update(configMap)
	if k8serrors.IsNotFound(err) {
		_, err = configMaps.Create(configMap)
	}
	return errors.Trace(err)
}

// maybeUpdatePodImage updates the pod image for the selector so long as its Juju version
// label matches the given version.
func (k *kubernetesClient) maybeUpdatePodImage(selector, jujuVersion, image string) error {
	pods := k.CoreV1().Pods(k.namespace)
	podList, err := pods.List(v1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return errors.Trace(err)
	}
	if len(podList.Items) == 0 {
		return errors.NotFoundf("pod %q", selector)
	}
	pod := podList.Items[0]

	// If the pod is for the same Juju version, we know the pod spec
	// will not have changed so can just update the image.
	// TODO(caas) - we should compare the old and new pod specs
	if pod.Labels[jujuVersion] != jujuVersion {
		return errors.New("version mismatch")
	}
	pod.Spec.Containers[0].Image = image
	_, err = pods.Update(&pod)
	return errors.Trace(err)
}

func (k *kubernetesClient) ensurePod(pod *core.Pod) error {
	// Kubernetes doesn't support updating a pod except under specific
	// circumstances so we need to delete and create.
	pods := k.CoreV1().Pods(k.namespace)
	if err := k.deletePod(pod.Name); err != nil {
		return errors.Trace(err)
	}
	_, err := pods.Create(pod)
	return errors.Trace(err)
}

func (k *kubernetesClient) deletePod(podName string) error {
	pods := k.CoreV1().Pods(k.namespace)
	err := pods.Delete(podName, &v1.DeleteOptions{
		PropagationPolicy: &defaultPropagationPolicy,
	})
	if k8serrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return errors.Trace(err)
	}

	// Wait for pod to be deleted.
	//
	// TODO(caas) if we even need to wait,
	// consider using pods.Watch.
	errExists := errors.New("exists")
	retryArgs := retry.CallArgs{
		Clock: clock.WallClock,
		IsFatalError: func(err error) bool {
			return errors.Cause(err) != errExists
		},
		Func: func() error {
			_, err := pods.Get(podName, v1.GetOptions{})
			if err == nil {
				return errExists
			}
			if k8serrors.IsNotFound(err) {
				return nil
			}
			return errors.Trace(err)
		},
		Delay:       5 * time.Second,
		MaxDuration: 2 * time.Minute,
	}
	return retry.Call(retryArgs)
}

// operatorPod returns a *core.Pod for the operator pod
// of the specified application.
func operatorPod(appName, agentPath, operatorImagePath, version string) *core.Pod {
	podName := operatorPodName(appName)
	configMapName := operatorConfigMapName(appName)
	configVolName := configMapName + "-volume"

	appTag := names.NewApplicationTag(appName)
	return &core.Pod{
		ObjectMeta: v1.ObjectMeta{
			Name:   podName,
			Labels: map[string]string{labelOperator: appName, labelVersion: version},
		},
		Spec: core.PodSpec{
			Containers: []core.Container{{
				Name:            "juju-operator",
				ImagePullPolicy: core.PullIfNotPresent,
				Image:           operatorImagePath,
				Env: []core.EnvVar{
					{Name: "JUJU_APPLICATION", Value: appName},
				},
				VolumeMounts: []core.VolumeMount{{
					Name:      configVolName,
					MountPath: agent.Dir(agentPath, appTag) + "/agent.conf",
					SubPath:   "agent.conf",
				}},
			}},
			Volumes: []core.Volume{{
				Name: configVolName,
				VolumeSource: core.VolumeSource{
					ConfigMap: &core.ConfigMapVolumeSource{
						LocalObjectReference: core.LocalObjectReference{
							Name: configMapName,
						},
						Items: []core.KeyToPath{{
							Key:  "agent.conf",
							Path: "agent.conf",
						}},
					},
				},
			}},
		},
	}
}

// operatorConfigMap returns a *core.ConfigMap for the operator pod
// of the specified application, with the specified configuration.
func operatorConfigMap(appName string, config *caas.OperatorConfig) *core.ConfigMap {
	configMapName := operatorConfigMapName(appName)
	return &core.ConfigMap{
		ObjectMeta: v1.ObjectMeta{
			Name: configMapName,
		},
		Data: map[string]string{
			"agent.conf": string(config.AgentConf),
		},
	}
}

type unitSpec struct {
	Pod core.PodSpec `json:"pod"`
}

var defaultPodTemplate = `
pod:
  containers:
  {{- range .Containers }}
  - name: {{.Name}}
    image: {{.Image}}
    {{if .Ports}}
    ports:
    {{- range .Ports }}
        - containerPort: {{.ContainerPort}}
          {{if .Name}}name: {{.Name}}{{end}}
          {{if .Protocol}}protocol: {{.Protocol}}{{end}}
    {{- end}}
    {{end}}
    {{if .Config}}
    env:
    {{- range $k, $v := .Config }}
        - name: {{$k}}
          value: {{$v}}
    {{- end}}
    {{end}}
  {{- end}}
`[1:]

func makeUnitSpec(podSpec *caas.PodSpec) (*unitSpec, error) {
	// Fill out the easy bits using a template.
	tmpl := template.Must(template.New("").Parse(defaultPodTemplate))
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, podSpec); err != nil {
		return nil, errors.Trace(err)
	}
	unitSpecString := buf.String()

	var unitSpec unitSpec
	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(unitSpecString), len(unitSpecString))
	if err := decoder.Decode(&unitSpec); err != nil {
		return nil, errors.Trace(err)
	}

	// Now fill in the hard bits progamatically.
	for i, c := range podSpec.Containers {
		if c.ProviderContainer == nil {
			continue
		}
		spec, ok := c.ProviderContainer.(*K8sContainerSpec)
		if !ok {
			return nil, errors.Errorf("unexpected kubernetes container spec type %T", c.ProviderContainer)
		}
		unitSpec.Pod.Containers[i].ImagePullPolicy = spec.ImagePullPolicy
		if spec.LivenessProbe != nil {
			unitSpec.Pod.Containers[i].LivenessProbe = spec.LivenessProbe
		}
		if spec.ReadinessProbe != nil {
			unitSpec.Pod.Containers[i].ReadinessProbe = spec.ReadinessProbe
		}
	}
	return &unitSpec, nil
}

func operatorPodName(appName string) string {
	return "juju-operator-" + appName
}

func operatorConfigMapName(appName string) string {
	return operatorPodName(appName) + "-config"
}

func applicationConfigMapName(appName, fileSetName string) string {
	return fmt.Sprintf("%v-%v-config", deploymentName(appName), fileSetName)
}

func unitConfigMapName(unitName, fileSetName string) string {
	return fmt.Sprintf("%v-%v-config", unitPodName(unitName), fileSetName)
}

func unitPodName(unitName string) string {
	return "juju-" + names.NewUnitTag(unitName).String()
}

func deploymentName(appName string) string {
	return "juju-" + appName
}

func resourceNamePrefix(appName string) string {
	return "juju-" + names.NewApplicationTag(appName).String() + "-"
}
