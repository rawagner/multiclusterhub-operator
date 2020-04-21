package multiclusterhub

import (
	"context"
	"crypto/rand"
	err "errors"
	"fmt"
	"math/big"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	storv1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/go-logr/logr"
	operatorsv1beta1 "github.com/open-cluster-management/multicloudhub-operator/pkg/apis/operators/v1beta1"
	"github.com/open-cluster-management/multicloudhub-operator/pkg/channel"
	"github.com/open-cluster-management/multicloudhub-operator/pkg/deploying"
	"github.com/open-cluster-management/multicloudhub-operator/pkg/helmrepo"
	"github.com/open-cluster-management/multicloudhub-operator/pkg/mcm"
	"github.com/open-cluster-management/multicloudhub-operator/pkg/rendering"
	"github.com/open-cluster-management/multicloudhub-operator/pkg/subscription"
	"github.com/open-cluster-management/multicloudhub-operator/pkg/utils"
	apiregistrationv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
)

const hubFinalizer = "finalizer.operators.open-cluster-management.io"

var log = logf.Log.WithName("controller_multiclusterhub")

/**
* USER ACTION REQUIRED: This is a scaffold file intended for the user to modify with their own Controller
* business logic.  Delete these comments after modifying this file.*
 */

// Add creates a new MultiClusterHub Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &ReconcileMultiClusterHub{client: mgr.GetClient(), scheme: mgr.GetScheme()}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("multiclusterhub-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for apiService deletions
	pred := predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return false },
		GenericFunc: func(e event.GenericEvent) bool { return false },
		UpdateFunc:  func(e event.UpdateEvent) bool { return false },
		DeleteFunc: func(e event.DeleteEvent) bool {
			labels := e.Meta.GetLabels()
			_, nameExists := labels["installer.name"]
			_, namespaceExists := labels["installer.namespace"]
			return nameExists && namespaceExists
		},
	}

	// Watch for changes to primary resource MultiClusterHub
	err = c.Watch(&source.Kind{Type: &operatorsv1beta1.MultiClusterHub{}}, &handler.EnqueueRequestForObject{}, predicate.GenerationChangedPredicate{})
	if err != nil {
		return err
	}

	// Watch for changes to secondary resource Pods and requeue the owner MultiClusterHub
	err = c.Watch(&source.Kind{Type: &appsv1.Deployment{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &operatorsv1beta1.MultiClusterHub{},
	})
	if err != nil {
		return err
	}

	err = c.Watch(
		&source.Kind{Type: &apiregistrationv1.APIService{}},
		handler.Funcs{
			DeleteFunc: func(e event.DeleteEvent, q workqueue.RateLimitingInterface) {
				labels := e.Meta.GetLabels()
				q.Add(reconcile.Request{NamespacedName: types.NamespacedName{
					Name:      labels["installer.name"],
					Namespace: labels["installer.namespace"],
				}})
			},
		},
		pred,
	)
	if err != nil {
		return err
	}

	return nil
}

// blank assignment to verify that ReconcileMultiClusterHub implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileMultiClusterHub{}

// ReconcileMultiClusterHub reconciles a MultiClusterHub object
type ReconcileMultiClusterHub struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client    client.Client
	CacheSpec utils.CacheSpec
	scheme    *runtime.Scheme
}

// Reconcile reads that state of the cluster for a MultiClusterHub object and makes changes based on the state read
// and what is in the MultiClusterHub.Spec
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileMultiClusterHub) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling MultiClusterHub")

	// Fetch the MultiClusterHub instance
	multiClusterHub := &operatorsv1beta1.MultiClusterHub{}
	err := r.client.Get(context.TODO(), request.NamespacedName, multiClusterHub)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			reqLogger.Info("MultiClusterHub resource not found. Ignoring since object must be deleted")
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		reqLogger.Error(err, "Failed to get MultiClusterHub CR")
		return reconcile.Result{}, err
	}

	// Check if the multiClusterHub instance is marked to be deleted, which is
	// indicated by the deletion timestamp being set.
	isHubMarkedToBeDeleted := multiClusterHub.GetDeletionTimestamp() != nil
	if isHubMarkedToBeDeleted {
		if contains(multiClusterHub.GetFinalizers(), hubFinalizer) {
			// Run finalization logic. If the finalization
			// logic fails, don't remove the finalizer so
			// that we can retry during the next reconciliation.
			if err := r.finalizeHub(reqLogger, multiClusterHub); err != nil {
				return reconcile.Result{}, err
			}

			// Remove hubFinalizer. Once all finalizers have been
			// removed, the object will be deleted.
			multiClusterHub.SetFinalizers(remove(multiClusterHub.GetFinalizers(), hubFinalizer))

			err := r.client.Update(context.TODO(), multiClusterHub)
			if err != nil {
				return reconcile.Result{}, err
			}
		}

		return reconcile.Result{}, nil
	}

	// Add finalizer for this CR
	if !contains(multiClusterHub.GetFinalizers(), hubFinalizer) {
		if err := r.addFinalizer(reqLogger, multiClusterHub); err != nil {
			return reconcile.Result{}, err
		}
	}

	var result *reconcile.Result
	result, err = r.setDefaults(multiClusterHub)
	if result != nil {
		return *result, err
	}

	if r.shouldReadManifestFile(multiClusterHub) {
		if ManifestFileExists(multiClusterHub.Spec.Version) {
			imageShaDigests, err := GetImageShaDigest(multiClusterHub.Spec.Version)
			if err != nil {
				log.Error(err, "manifest file exists for given version, but could not get image sha digests")
				return reconcile.Result{}, err
			}
			r.CacheSpec.ImageShaDigests = imageShaDigests
			r.CacheSpec.ISDVersion = multiClusterHub.Spec.Version
		}
	}

	result, err = r.ensureDeployment(multiClusterHub, helmrepo.Deployment(multiClusterHub, r.CacheSpec))
	if result != nil {
		return *result, err
	}

	result, err = r.ensureService(multiClusterHub, helmrepo.Service(multiClusterHub))
	if result != nil {
		return *result, err
	}

	result, err = r.ensureObject(multiClusterHub, channel.Channel(multiClusterHub), channel.Schema)
	if result != nil {
		return *result, err
	}

	if multiClusterHub.Spec.CloudPakCompatibility {
		result, err = r.copyPullSecret(multiClusterHub, utils.CertManagerNamespace)
		if result != nil {
			return *result, err
		}
	}

	result, err = r.ensureObject(multiClusterHub, subscription.CertManager(multiClusterHub, r.CacheSpec), subscription.Schema)
	if result != nil {
		return *result, err
	}

	certGV := schema.GroupVersion{Group: "certmanager.k8s.io", Version: "v1alpha1"}
	result, err = r.apiReady(certGV)
	if result != nil {
		return *result, err
	}

	result, err = r.ensureObject(multiClusterHub, subscription.CertWebhook(multiClusterHub, r.CacheSpec), subscription.Schema)
	if result != nil {
		return *result, err
	}

	result, err = r.ensureObject(multiClusterHub, subscription.ConfigWatcher(multiClusterHub, r.CacheSpec), subscription.Schema)
	if result != nil {
		return *result, err
	}

	result, err = r.ensureSecret(multiClusterHub, r.mongoAuthSecret(multiClusterHub))
	if result != nil {
		return *result, err
	}

	result, err = r.ingressDomain(multiClusterHub)
	if result != nil {
		return *result, err
	}

	//Render the templates with a specified CR
	renderer := rendering.NewRenderer(multiClusterHub, r.CacheSpec)
	toDeploy, err := renderer.Render(r.client)
	if err != nil {
		reqLogger.Error(err, "Failed to render MultiClusterHub templates")
		return reconcile.Result{}, err
	}
	//Deploy the resources
	for _, res := range toDeploy {
		if res.GetNamespace() == multiClusterHub.Namespace {
			if err := controllerutil.SetControllerReference(multiClusterHub, res, r.scheme); err != nil {
				reqLogger.Error(err, "Failed to set controller reference")
			}
		}
		if err := deploying.Deploy(r.client, res); err != nil {
			reqLogger.Error(err, fmt.Sprintf("Failed to deploy %s %s/%s", res.GetKind(), multiClusterHub.Namespace, res.GetName()))
			return reconcile.Result{}, err
		}
	}

	// Install the rest of the subscriptions in no particular order
	result, err = r.ensureObject(multiClusterHub, subscription.ManagementIngress(multiClusterHub, r.CacheSpec), subscription.Schema)
	if result != nil {
		return *result, err
	}
	result, err = r.ensureObject(multiClusterHub, subscription.ApplicationUI(multiClusterHub, r.CacheSpec), subscription.Schema)
	if result != nil {
		return *result, err
	}
	result, err = r.ensureObject(multiClusterHub, subscription.Console(multiClusterHub, r.CacheSpec), subscription.Schema)
	if result != nil {
		return *result, err
	}
	result, err = r.ensureObject(multiClusterHub, subscription.GRC(multiClusterHub, r.CacheSpec), subscription.Schema)
	if result != nil {
		return *result, err
	}
	result, err = r.ensureObject(multiClusterHub, subscription.KUIWebTerminal(multiClusterHub, r.CacheSpec), subscription.Schema)
	if result != nil {
		return *result, err
	}
	result, err = r.ensureObject(multiClusterHub, subscription.MongoDB(multiClusterHub, r.CacheSpec), subscription.Schema)
	if result != nil {
		return *result, err
	}
	result, err = r.ensureObject(multiClusterHub, subscription.RCM(multiClusterHub, r.CacheSpec), subscription.Schema)
	if result != nil {
		return *result, err
	}
	result, err = r.ensureObject(multiClusterHub, subscription.Search(multiClusterHub, r.CacheSpec), subscription.Schema)
	if result != nil {
		return *result, err
	}
	result, err = r.ensureObject(multiClusterHub, subscription.Topology(multiClusterHub, r.CacheSpec), subscription.Schema)
	if result != nil {
		return *result, err
	}

	result, err = r.ensureDeployment(multiClusterHub, mcm.APIServerDeployment(multiClusterHub, r.CacheSpec))
	if result != nil {
		return *result, err
	}

	result, err = r.ensureService(multiClusterHub, mcm.APIServerService(multiClusterHub))
	if result != nil {
		return *result, err
	}

	result, err = r.ensureDeployment(multiClusterHub, mcm.WebhookDeployment(multiClusterHub, r.CacheSpec))
	if result != nil {
		return *result, err
	}

	result, err = r.ensureService(multiClusterHub, mcm.WebhookService(multiClusterHub))
	if result != nil {
		return *result, err
	}

	result, err = r.ensureDeployment(multiClusterHub, mcm.ControllerDeployment(multiClusterHub, r.CacheSpec))
	if result != nil {
		return *result, err
	}

	// Update the CR status
	multiClusterHub.Status.Phase = "Pending"
	ready, deployments, err := deploying.ListDeployments(r.client, multiClusterHub.Namespace)
	if err != nil {
		reqLogger.Error(err, "Failed to list deployments")
		return reconcile.Result{}, err
	}
	if ready {
		multiClusterHub.Status.Phase = "Running"
	}
	statedDeployments := []operatorsv1beta1.DeploymentResult{}
	for _, deploy := range deployments {
		statedDeployments = append(statedDeployments, operatorsv1beta1.DeploymentResult{
			Name:   deploy.Name,
			Status: deploy.Status,
		})
	}
	multiClusterHub.Status.Deployments = statedDeployments

	err = r.client.Status().Update(context.TODO(), multiClusterHub)
	if err != nil {
		if errors.IsConflict(err) {
			// Error from object being modified is normal behavior and should not be treated like an error
			reqLogger.Info("Failed to update status", "Reason", "Object has been modified")
			return reconcile.Result{RequeueAfter: time.Second}, nil
		}

		reqLogger.Error(err, fmt.Sprintf("Failed to update %s/%s status ", multiClusterHub.Namespace, multiClusterHub.Name))
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

func (r *ReconcileMultiClusterHub) mongoAuthSecret(v *operatorsv1beta1.MultiClusterHub) *corev1.Secret {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mongodb-admin",
			Namespace: v.Namespace,
		},
		Type: "Opaque",
		StringData: map[string]string{
			"user":     "admin",
			"password": generatePass(16),
		},
	}

	if err := controllerutil.SetControllerReference(v, secret, r.scheme); err != nil {
		log.Error(err, "Failed to set controller reference", "Secret.Namespace", v.Namespace, "Secret.Name", v.Name)
	}
	return secret
}

func generatePass(length int) string {
	chars := "ABCDEFGHIJKLMNOPQRSTUVWXYZ" +
		"abcdefghijklmnopqrstuvwxyz" +
		"0123456789"

	buf := make([]byte, length)
	for i := 0; i < length; i++ {
		nBig, _ := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		buf[i] = chars[nBig.Int64()]
	}
	return string(buf)
}

// setDefaults updates MultiClusterHub resource with proper defaults
func (r *ReconcileMultiClusterHub) setDefaults(m *operatorsv1beta1.MultiClusterHub) (*reconcile.Result, error) {
	if utils.MchIsValid(m) {
		return nil, nil
	}
	log.Info("MultiClusterHub is Invalid. Updating with proper defaults")

	if m.Spec.Version == "" {
		componentVersion, err := r.ReadComponentVersionFile()
		if err != nil {
			return &reconcile.Result{}, err
		}
		m.Spec.Version = componentVersion
	}

	if !utils.IsVersionSupported(m.Spec.Version) {
		err := err.New("Version " + m.Spec.Version + " not supported")
		log.Error(err, "Overriding with valid version")
		componentVersion, err := r.ReadComponentVersionFile()
		if err != nil {
			return &reconcile.Result{}, err
		}
		m.Spec.Version = componentVersion
	}

	if m.Spec.ImageRepository == "" {
		m.Spec.ImageRepository = utils.DefaultRepository
	}

	if m.Spec.ImagePullPolicy == "" {
		m.Spec.ImagePullPolicy = corev1.PullAlways
	}

	if m.Spec.Mongo.Storage == "" {
		m.Spec.Mongo.Storage = "5Gi"
	}

	if m.Spec.Mongo.StorageClass == "" {
		storageClass, err := r.getStorageClass()
		if err != nil {
			return &reconcile.Result{}, err
		}
		m.Spec.Mongo.StorageClass = storageClass
	}

	if m.Spec.Etcd.Storage == "" {
		m.Spec.Etcd.Storage = "1Gi"
	}

	if m.Spec.Etcd.StorageClass == "" {
		storageClass, err := r.getStorageClass()
		if err != nil {
			return &reconcile.Result{}, err
		}
		m.Spec.Etcd.StorageClass = storageClass
	}

	replicas := int(1)
	if m.Spec.ReplicaCount == nil {
		m.Spec.ReplicaCount = &replicas
	} else if *m.Spec.ReplicaCount <= 0 {
		m.Spec.ReplicaCount = &replicas
	}

	mongoReplicas := int(3)
	if m.Spec.Mongo.ReplicaCount == nil {
		m.Spec.Mongo.ReplicaCount = &mongoReplicas
	} else if *m.Spec.Mongo.ReplicaCount <= 0 {
		m.Spec.Mongo.ReplicaCount = &mongoReplicas
	}

	// Apply defaults to server
	err := r.client.Update(context.TODO(), m)
	if err != nil {
		log.Error(err, "Failed to update MultiClusterHub", "MultiClusterHub.Namespace", m.Namespace, "MultiClusterHub.Name", m.Name)
		return &reconcile.Result{}, err
	}

	log.Info("MultiClusterHub successfully updated")
	return &reconcile.Result{Requeue: true}, nil
}

// getStorageClass retrieves the default storage class if it exists
func (r *ReconcileMultiClusterHub) getStorageClass() (string, error) {
	scList := &storv1.StorageClassList{}
	if err := r.client.List(context.TODO(), scList); err != nil {
		return "", err
	}
	for _, sc := range scList.Items {
		if sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" {
			return sc.GetName(), nil
		}
	}
	return "", fmt.Errorf("failed to find default storageclass")
}

// ingressDomain is discovered from Openshift cluster configuration resources
func (r *ReconcileMultiClusterHub) ingressDomain(m *operatorsv1beta1.MultiClusterHub) (*reconcile.Result, error) {
	if r.CacheSpec.IngressDomain != "" {
		return nil, nil
	}

	// Create dynamic client
	dc, err := createDynamicClient()
	if err != nil {
		log.Error(err, "Failed to create dynamic client")
		return &reconcile.Result{}, err
	}

	// Find resource
	schema := schema.GroupVersionResource{Group: "config.openshift.io", Version: "v1", Resource: "ingresses"}
	crd, err := dc.Resource(schema).Get("cluster", metav1.GetOptions{})
	if err != nil {
		log.Error(err, "Failed to get resource", "resource", schema.GroupResource().String())
		return &reconcile.Result{}, err
	}

	// Parse resource for domain value
	domain, ok, err := unstructured.NestedString(crd.UnstructuredContent(), "spec", "domain")
	if err != nil {
		log.Error(err, "Error parsing resource", "resource", schema.GroupResource().String(), "value", "spec.domain")
		return &reconcile.Result{}, err
	}
	if !ok {
		err = fmt.Errorf("field not found")
		log.Error(err, "Ingress config did not contain expected value", "resource", schema.GroupResource().String(), "value", "spec.domain")
		return &reconcile.Result{}, err
	}

	log.Info("Ingress domain not set, updating value in cachespec", "MultiClusterHub.Namespace", m.Namespace, "MultiClusterHub.Name", m.Name, "ingressDomain", domain)
	r.CacheSpec.IngressDomain = domain
	return nil, nil
}

func (r *ReconcileMultiClusterHub) finalizeHub(reqLogger logr.Logger, m *operatorsv1beta1.MultiClusterHub) error {
	if err := r.cleanupHiveConfigs(reqLogger, m); err != nil {
		return err
	}
	if err := r.cleanupAPIServices(reqLogger, m); err != nil {
		return err
	}
	if err := r.cleanupClusterRoles(reqLogger, m); err != nil {
		return err
	}
	if err := r.cleanupClusterRoleBindings(reqLogger, m); err != nil {
		return err
	}
	if err := r.cleanupMutatingWebhooks(reqLogger, m); err != nil {
		return err
	}
	if err := r.cleanupCRDs(reqLogger, m); err != nil {
		return err
	}
	if m.Spec.CloudPakCompatibility {
		if err := r.cleanupPullSecret(reqLogger, m); err != nil {
			return err
		}
	}

	reqLogger.Info("Successfully finalized multiClusterHub")
	return nil
}

func (r *ReconcileMultiClusterHub) addFinalizer(reqLogger logr.Logger, m *operatorsv1beta1.MultiClusterHub) error {
	reqLogger.Info("Adding Finalizer for the multiClusterHub")
	m.SetFinalizers(append(m.GetFinalizers(), hubFinalizer))

	// Update CR
	err := r.client.Update(context.TODO(), m)
	if err != nil {
		reqLogger.Error(err, "Failed to update MultiClusterHub with finalizer")
		return err
	}
	return nil
}

func (r *ReconcileMultiClusterHub) shouldReadManifestFile(m *operatorsv1beta1.MultiClusterHub) bool {
	// read manifest file if:

	// (1) CacheSpec.ImageShaDigests doesn't exist or
	if r.CacheSpec.ImageShaDigests == nil {
		return true
	}
	// (2) CacheSpec.ISDVersion doesn't exist or
	if r.CacheSpec.ISDVersion == "" {
		return true
	}
	// (3) CacheSpec.ISDVersion is not up-to-date with spec.Version
	if r.CacheSpec.ISDVersion != m.Spec.Version {
		return true
	}
	return false
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func remove(list []string, s string) []string {
	for i, v := range list {
		if v == s {
			list = append(list[:i], list[i+1:]...)
		}
	}
	return list
}
