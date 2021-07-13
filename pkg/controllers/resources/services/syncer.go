package services

import (
	"context"
	context2 "github.com/loft-sh/vcluster/cmd/vcluster/context"
	"github.com/loft-sh/vcluster/pkg/controllers/resources/generic"
	"github.com/loft-sh/vcluster/pkg/util/loghelper"
	"github.com/loft-sh/vcluster/pkg/util/translate"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sync"
	"time"
)

func RegisterIndices(ctx *context2.ControllerContext) error {
	err := generic.RegisterSyncerIndices(ctx, &corev1.Service{})
	if err != nil {
		return err
	}

	return nil
}

func Register(ctx *context2.ControllerContext) error {
	var err error
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: kubernetes.NewForConfigOrDie(ctx.VirtualManager.GetConfig()).CoreV1().Events("")})

	serviceClient := ctx.LocalManager.GetClient()
	if ctx.Options.ServiceNamespace != ctx.Options.TargetNamespace {
		serviceClient, err = client.New(ctx.LocalManager.GetConfig(), client.Options{
			Scheme: ctx.LocalManager.GetScheme(),
			Mapper: ctx.LocalManager.GetRESTMapper(),
		})
		if err != nil {
			return errors.Wrap(err, "create uncached client")
		}
	}

	return generic.RegisterSyncer(ctx, &syncer{
		eventRecoder:     eventBroadcaster.NewRecorder(ctx.VirtualManager.GetScheme(), corev1.EventSource{Component: "service-syncer"}),
		targetNamespace:  ctx.Options.TargetNamespace,
		serviceNamespace: ctx.Options.ServiceNamespace,
		serviceName:      ctx.Options.ServiceName,
		serviceClient:    serviceClient,
		localClient:      ctx.LocalManager.GetClient(),
		virtualClient:    ctx.VirtualManager.GetClient(),
	}, "service", generic.RegisterSyncerOptions{})
}

type syncer struct {
	syncMutex    sync.Mutex
	eventRecoder record.EventRecorder

	targetNamespace string

	serviceClient    client.Client
	serviceNamespace string
	serviceName      string

	localClient   client.Client
	virtualClient client.Client
}

func (s *syncer) New() client.Object {
	return &corev1.Service{}
}

func (s *syncer) NewList() client.ObjectList {
	return &corev1.ServiceList{}
}

func (s *syncer) ForwardCreate(ctx context.Context, vObj client.Object, log loghelper.Logger) (ctrl.Result, error) {
	vService := vObj.(*corev1.Service)

	newObj, err := translate.SetupMetadata(s.targetNamespace, vService)
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "error setting metadata")
	}

	newService := newObj.(*corev1.Service)
	newService.Spec.Selector = nil
	newService.Spec.ClusterIP = ""
	newService.Spec.ClusterIPs = nil
	log.Infof("create physical service %s/%s", newService.Namespace, newService.Name)
	err = s.localClient.Create(ctx, newService)
	if err != nil {
		log.Infof("error syncing %s/%s to physical cluster: %v", vService.Namespace, vService.Name, err)
		s.eventRecoder.Eventf(vService, "Warning", "SyncError", "Error syncing to physical cluster: %v", err)
		return ctrl.Result{RequeueAfter: time.Second}, err
	}

	return ctrl.Result{}, nil
}

func (s *syncer) ForwardCreateNeeded(vObj client.Object) (bool, error) {
	// dont do anything for the kubernetes service
	vService := vObj.(*corev1.Service)
	if vService.Name == "kubernetes" && vService.Namespace == "default" {
		return false, nil
	}

	return true, nil
}

func (s *syncer) ForwardUpdate(ctx context.Context, pObj client.Object, vObj client.Object, log loghelper.Logger) (ctrl.Result, error) {
	var err error
	vService := vObj.(*corev1.Service)
	pService := pObj.(*corev1.Service)

	// delay if we are in the middle of a switch operation
	if isSwitchingFromExternalName(pService, vService) {
		return ctrl.Result{RequeueAfter: time.Second * 3}, nil
	}

	// did the service change?
	updated := calcServiceDiff(pService, vService)
	if updated != nil {
		log.Infof("updating physical service %s/%s, because virtual service has changed", updated.Namespace, updated.Name)
		err = s.localClient.Update(ctx, updated)
		if err != nil {
			s.eventRecoder.Eventf(vService, "Warning", "SyncError", "Error syncing to physical cluster: %v", err)
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (s *syncer) ForwardUpdateNeeded(pObj client.Object, vObj client.Object) (bool, error) {
	vService := vObj.(*corev1.Service)
	pService := pObj.(*corev1.Service)
	updated := calcServiceDiff(pService, vService)
	return updated != nil, nil
}

func calcServiceDiff(pObj, vObj *corev1.Service) *corev1.Service {
	var updated *corev1.Service

	// check ports
	if !equality.Semantic.DeepEqual(vObj.Spec.Ports, pObj.Spec.Ports) {
		updated = pObj.DeepCopy()
		updated.Spec.Ports = vObj.Spec.Ports
	}

	// check annotations
	if !equality.Semantic.DeepEqual(vObj.Annotations, pObj.Annotations) {
		if updated == nil {
			updated = pObj.DeepCopy()
		}
		updated.Annotations = vObj.Annotations
	}

	// check labels
	if !translate.LabelsEqual(vObj.Namespace, vObj.Labels, pObj.Labels) {
		if updated == nil {
			updated = pObj.DeepCopy()
		}
		updated.Labels = translate.TranslateLabels(vObj.Namespace, vObj.Labels)
	}

	// publish not ready addresses
	if vObj.Spec.PublishNotReadyAddresses != pObj.Spec.PublishNotReadyAddresses {
		if updated == nil {
			updated = pObj.DeepCopy()
		}
		updated.Spec.PublishNotReadyAddresses = vObj.Spec.PublishNotReadyAddresses
	}

	// type
	if vObj.Spec.Type != pObj.Spec.Type {
		if updated == nil {
			updated = pObj.DeepCopy()
		}
		updated.Spec.Type = vObj.Spec.Type
	}

	// external name
	if vObj.Spec.ExternalName != pObj.Spec.ExternalName {
		if updated == nil {
			updated = pObj.DeepCopy()
		}
		updated.Spec.ExternalName = vObj.Spec.ExternalName
	}

	// externalTrafficPolicy
	if vObj.Spec.ExternalTrafficPolicy != pObj.Spec.ExternalTrafficPolicy {
		if updated == nil {
			updated = pObj.DeepCopy()
		}
		updated.Spec.ExternalTrafficPolicy = vObj.Spec.ExternalTrafficPolicy
	}

	// session affinity
	if vObj.Spec.SessionAffinity != pObj.Spec.SessionAffinity {
		if updated == nil {
			updated = pObj.DeepCopy()
		}
		updated.Spec.SessionAffinity = vObj.Spec.SessionAffinity
	}

	// sessionAffinityConfig
	if !equality.Semantic.DeepEqual(vObj.Spec.SessionAffinityConfig, pObj.Spec.SessionAffinityConfig) {
		if updated == nil {
			updated = pObj.DeepCopy()
		}
		updated.Spec.SessionAffinityConfig = vObj.Spec.SessionAffinityConfig
	}

	// healthCheckNodePort
	if vObj.Spec.HealthCheckNodePort != pObj.Spec.HealthCheckNodePort {
		if updated == nil {
			updated = pObj.DeepCopy()
		}
		updated.Spec.HealthCheckNodePort = vObj.Spec.HealthCheckNodePort
	}

	// TopologyKeys
	if !equality.Semantic.DeepEqual(vObj.Spec.TopologyKeys, pObj.Spec.TopologyKeys) {
		if updated == nil {
			updated = pObj.DeepCopy()
		}
		updated.Spec.TopologyKeys = vObj.Spec.TopologyKeys
	}

	return updated
}

func isSwitchingFromExternalName(pService *corev1.Service, vService *corev1.Service) bool {
	return vService.Spec.Type == corev1.ServiceTypeExternalName && pService.Spec.Type != vService.Spec.Type && pService.Spec.ClusterIP != ""
}

func (s *syncer) BackwardUpdate(ctx context.Context, pObj client.Object, vObj client.Object, log loghelper.Logger) (ctrl.Result, error) {
	vService := vObj.(*corev1.Service)
	pService := pObj.(*corev1.Service)

	var err error
	if serviceSpecUpdateNeeded(vService, pService) {
		if isSwitchingFromExternalName(pService, vService) {
			return ctrl.Result{RequeueAfter: time.Second * 3}, nil
		}

		newService := vService.DeepCopy()
		newService.Spec.ClusterIP = pService.Spec.ClusterIP
		newService.Spec.ExternalName = pService.Spec.ExternalName
		newService.Spec.ExternalIPs = pService.Spec.ExternalIPs
		newService.Spec.LoadBalancerIP = pService.Spec.LoadBalancerIP
		newService.Spec.LoadBalancerSourceRanges = pService.Spec.LoadBalancerSourceRanges
		if vService.Spec.ClusterIP != pService.Spec.ClusterIP {
			newService.Spec.ClusterIPs = nil
			log.Infof("recreating virtual service %s/%s, because cluster ip differs %s != %s", vService.Namespace, vService.Name, pService.Spec.ClusterIP, vService.Spec.ClusterIP)

			// recreate the new service with the correct cluster ip
			newService, err = recreateService(ctx, s.virtualClient, newService)
			if err != nil {
				log.Errorf("error creating virtual service: %s/%s", vService.Namespace, vService.Name)
				return ctrl.Result{}, err
			}
		} else {
			// update with correct ports
			log.Infof("update virtual service %s/%s, because spec is out of sync", vService.Namespace, vService.Name)
			err = s.virtualClient.Update(ctx, newService)
			if err != nil {
				return ctrl.Result{}, err
			}
		}

		vService = newService
	}

	if !equality.Semantic.DeepEqual(vService.Status, pService.Status) {
		newService := vService.DeepCopy()
		newService.Status = pService.Status
		log.Infof("update virtual service %s/%s, because status is out of sync", vService.Namespace, vService.Name)
		err = s.virtualClient.Status().Update(ctx, newService)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (s *syncer) BackwardUpdateNeeded(pObj client.Object, vObj client.Object) (bool, error) {
	vService := vObj.(*corev1.Service)
	pService := pObj.(*corev1.Service)

	return serviceSpecUpdateNeeded(vService, pService) || !equality.Semantic.DeepEqual(vService.Status, pService.Status), nil
}

func (s *syncer) BackwardStart(ctx context.Context, req ctrl.Request) (bool, error) {
	s.syncMutex.Lock()

	// sync the kubernetes service
	if req.Name == s.serviceName && req.Namespace == s.serviceNamespace {
		return true, SyncKubernetesService(ctx, s.virtualClient, s.serviceClient, s.serviceNamespace, s.serviceName)
	}

	return false, nil
}

func (s *syncer) BackwardEnd() {
	s.syncMutex.Unlock()
}

func (s *syncer) ForwardStart(ctx context.Context, req ctrl.Request) (bool, error) {
	s.syncMutex.Lock()

	// dont do anything for the kubernetes service
	if req.Name == "kubernetes" && req.Namespace == "default" {
		return true, SyncKubernetesService(ctx, s.virtualClient, s.serviceClient, s.serviceNamespace, s.serviceName)
	}

	return false, nil
}

func (s *syncer) ForwardEnd() {
	s.syncMutex.Unlock()
}

func (s *syncer) BackwardDelete(ctx context.Context, pObj client.Object, log loghelper.Logger) (ctrl.Result, error) {
	// we have to delay deletion here if a vObj does not (yet) exist for a service that was just
	// created, because vcluster intercepts those calls and first creates a service inside the host
	// cluster and then inside the virtual cluster.
	//
	// We also don't need to care about the forwarding part deleting the physical object, because as soon as
	// that controller gets a delete event for a virtual service, we can safely delete the physical object.
	pService := pObj.(*corev1.Service)
	if pService.DeletionTimestamp == nil && pService.CreationTimestamp.Add(time.Second*180).After(time.Now()) {
		return ctrl.Result{RequeueAfter: time.Second * 10}, nil
	}

	return generic.DeleteObject(ctx, s.localClient, pObj, log)
}

func serviceSpecUpdateNeeded(vService *corev1.Service, pService *corev1.Service) bool {
	return vService.Spec.ClusterIP != pService.Spec.ClusterIP ||
		!equality.Semantic.DeepEqual(vService.Spec.ExternalIPs, pService.Spec.ExternalIPs) ||
		vService.Spec.LoadBalancerIP != pService.Spec.LoadBalancerIP ||
		!equality.Semantic.DeepEqual(vService.Spec.LoadBalancerSourceRanges, pService.Spec.LoadBalancerSourceRanges)
}

func recreateService(ctx context.Context, virtualClient client.Client, vService *corev1.Service) (*corev1.Service, error) {
	// delete & create with correct ClusterIP
	err := virtualClient.Delete(ctx, vService)
	if err != nil && kerrors.IsNotFound(err) == false {
		return nil, err
	}

	// make sure we don't set the resource version during create
	vService = vService.DeepCopy()
	vService.ResourceVersion = ""
	vService.UID = ""
	vService.DeletionTimestamp = nil
	vService.Generation = 0

	// create the new service with the correct cluster ip
	err = virtualClient.Create(ctx, vService)
	if err != nil {
		klog.Errorf("error recreating virtual service: %s/%s: %v", vService.Namespace, vService.Name, err)
		return nil, err
	}

	return vService, nil
}

func SyncKubernetesService(ctx context.Context, virtualClient client.Client, localClient client.Client, serviceNamespace, serviceName string) error {
	// get physical service
	pObj := &corev1.Service{}
	err := localClient.Get(ctx, types.NamespacedName{
		Namespace: serviceNamespace,
		Name:      serviceName,
	}, pObj)
	if err != nil {
		if kerrors.IsNotFound(err) {
			return nil
		}

		return err
	}

	// get virtual service
	vObj := &corev1.Service{}
	err = virtualClient.Get(ctx, types.NamespacedName{
		Namespace: "default",
		Name:      "kubernetes",
	}, vObj)
	if err != nil {
		if kerrors.IsNotFound(err) {
			return nil
		}

		return err
	}

	translatedPorts := translateKubernetesServicePorts(pObj.Spec.Ports)
	if vObj.Spec.ClusterIP != pObj.Spec.ClusterIP || !equality.Semantic.DeepEqual(vObj.Spec.Ports, translatedPorts) {
		newService := vObj.DeepCopy()
		newService.Spec.ClusterIP = pObj.Spec.ClusterIP
		newService.Spec.Ports = translatedPorts
		if vObj.Spec.ClusterIP != pObj.Spec.ClusterIP {
			newService.Spec.ClusterIPs = nil

			// delete & create with correct ClusterIP
			err = virtualClient.Delete(ctx, vObj)
			if err != nil {
				return err
			}

			// make sure we don't set the resource version during create
			newService.ResourceVersion = ""

			// create the new service with the correct cluster ip
			err = virtualClient.Create(ctx, newService)
			if err != nil {
				return err
			}
		} else {
			// delete & create with correct ClusterIP
			err = virtualClient.Update(ctx, newService)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func translateKubernetesServicePorts(ports []corev1.ServicePort) []corev1.ServicePort {
	retPorts := []corev1.ServicePort{}
	for _, p := range ports {
		// Delete the NodePort
		retPorts = append(retPorts, corev1.ServicePort{
			Name:        p.Name,
			Protocol:    p.Protocol,
			AppProtocol: p.AppProtocol,
			Port:        p.Port,
			TargetPort:  p.TargetPort,
		})
	}

	return retPorts
}
