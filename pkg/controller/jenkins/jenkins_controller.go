package jenkins

import (
	"context"
	"fmt"

	"github.com/oldsj/jenkins-operator/pkg/apis/jenkinsio/v1alpha1"
	"github.com/oldsj/jenkins-operator/pkg/controller/jenkins/configuration/base"
	"github.com/oldsj/jenkins-operator/pkg/controller/jenkins/configuration/user"
	"github.com/oldsj/jenkins-operator/pkg/controller/jenkins/constants"
	"github.com/oldsj/jenkins-operator/pkg/controller/jenkins/plugins"
	"github.com/oldsj/jenkins-operator/pkg/event"
	"github.com/oldsj/jenkins-operator/pkg/log"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	// reasonBaseConfigurationSuccess is the event which informs base configuration has been completed successfully
	reasonBaseConfigurationSuccess event.Reason = "BaseConfigurationSuccess"
	// reasonUserConfigurationSuccess is the event which informs user configuration has been completed successfully
	reasonUserConfigurationSuccess event.Reason = "BaseConfigurationFailure"
	// reasonCRValidationFailure is the event which informs user has provided invalid configuration in Jenkins CR
	reasonCRValidationFailure event.Reason = "CRValidationFailure"
)

// Add creates a new Jenkins Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager, local, minikube bool, events event.Recorder) error {
	return add(mgr, newReconciler(mgr, local, minikube, events))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager, local, minikube bool, events event.Recorder) reconcile.Reconciler {
	return &ReconcileJenkins{
		client:   mgr.GetClient(),
		scheme:   mgr.GetScheme(),
		local:    local,
		minikube: minikube,
		events:   events,
	}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("jenkins-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return errors.WithStack(err)
	}

	// Watch for changes to primary resource Jenkins
	err = c.Watch(&source.Kind{Type: &v1alpha1.Jenkins{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return errors.WithStack(err)
	}

	// Watch for changes to secondary resource Pods and requeue the owner Jenkins
	err = c.Watch(&source.Kind{Type: &corev1.Pod{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &v1alpha1.Jenkins{},
	})
	if err != nil {
		return errors.WithStack(err)
	}

	jenkinsHandler := &enqueueRequestForJenkins{}
	err = c.Watch(&source.Kind{Type: &corev1.Secret{}}, jenkinsHandler)
	if err != nil {
		return errors.WithStack(err)
	}

	err = c.Watch(&source.Kind{Type: &corev1.ConfigMap{}}, jenkinsHandler)
	if err != nil {
		return errors.WithStack(err)
	}

	return nil
}

var _ reconcile.Reconciler = &ReconcileJenkins{}

// ReconcileJenkins reconciles a Jenkins object
type ReconcileJenkins struct {
	client          client.Client
	scheme          *runtime.Scheme
	local, minikube bool
	events          event.Recorder
}

// Reconcile it's a main reconciliation loop which maintain desired state based on Jenkins.Spec
func (r *ReconcileJenkins) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	logger := r.buildLogger(request.Name)
	logger.V(log.VDebug).Info("Reconciling Jenkins")

	result, err := r.reconcile(request, logger)
	if err != nil && apierrors.IsConflict(err) {
		logger.V(log.VWarn).Info(err.Error())
		return reconcile.Result{Requeue: true}, nil
	} else if err != nil {
		if log.Debug {
			logger.V(log.VWarn).Info(fmt.Sprintf("Reconcile loop failed: %+v", err))
		} else {
			logger.V(log.VWarn).Info(fmt.Sprintf("Reconcile loop failed: %s", err))
		}
		return reconcile.Result{Requeue: true}, nil
	}
	return result, nil
}

func (r *ReconcileJenkins) reconcile(request reconcile.Request, logger logr.Logger) (reconcile.Result, error) {
	// Fetch the Jenkins instance
	jenkins := &v1alpha1.Jenkins{}
	err := r.client.Get(context.TODO(), request.NamespacedName, jenkins)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, errors.WithStack(err)
	}

	err = r.setDefaults(jenkins, logger)
	if err != nil {
		return reconcile.Result{}, err
	}

	// Reconcile base configuration
	baseConfiguration := base.New(r.client, r.scheme, logger, jenkins, r.local, r.minikube)

	valid, err := baseConfiguration.Validate(jenkins)
	if err != nil {
		return reconcile.Result{}, err
	}
	if !valid {
		r.events.Emit(jenkins, event.TypeWarning, reasonCRValidationFailure, "Base CR validation failed")
		logger.V(log.VWarn).Info("Validation of base configuration failed, please correct Jenkins CR")
		return reconcile.Result{}, nil // don't requeue
	}

	result, jenkinsClient, err := baseConfiguration.Reconcile()
	if err != nil {
		return reconcile.Result{}, err
	}
	if result.Requeue {
		return result, nil
	}

	if jenkins.Status.BaseConfigurationCompletedTime == nil {
		now := metav1.Now()
		jenkins.Status.BaseConfigurationCompletedTime = &now
		err = r.client.Update(context.TODO(), jenkins)
		if err != nil {
			return reconcile.Result{}, errors.WithStack(err)
		}
		logger.Info(fmt.Sprintf("Base configuration phase is complete, took %s",
			jenkins.Status.BaseConfigurationCompletedTime.Sub(jenkins.Status.ProvisionStartTime.Time)))
		r.events.Emit(jenkins, event.TypeNormal, reasonBaseConfigurationSuccess, "Base configuration completed")
	}
	// Reconcile user configuration
	userConfiguration := user.New(r.client, jenkinsClient, logger, jenkins)

	valid, err = userConfiguration.Validate(jenkins)
	if err != nil {
		return reconcile.Result{}, err
	}
	if !valid {
		logger.V(log.VWarn).Info("Validation of user configuration failed, please correct Jenkins CR")
		r.events.Emit(jenkins, event.TypeWarning, reasonCRValidationFailure, "User CR validation failed")
		return reconcile.Result{}, nil // don't requeue
	}

	result, err = userConfiguration.Reconcile()
	if err != nil {
		return reconcile.Result{}, err
	}
	if result.Requeue {
		return result, nil
	}

	if jenkins.Status.UserConfigurationCompletedTime == nil {
		now := metav1.Now()
		jenkins.Status.UserConfigurationCompletedTime = &now
		err = r.client.Update(context.TODO(), jenkins)
		if err != nil {
			return reconcile.Result{}, errors.WithStack(err)
		}
		logger.Info(fmt.Sprintf("User configuration phase is complete, took %s",
			jenkins.Status.UserConfigurationCompletedTime.Sub(jenkins.Status.ProvisionStartTime.Time)))
		r.events.Emit(jenkins, event.TypeNormal, reasonUserConfigurationSuccess, "User configuration completed")
	}

	return reconcile.Result{}, nil
}

func (r *ReconcileJenkins) buildLogger(jenkinsName string) logr.Logger {
	return log.Log.WithValues("cr", jenkinsName)
}

func (r *ReconcileJenkins) setDefaults(jenkins *v1alpha1.Jenkins, logger logr.Logger) error {
	changed := false
	if len(jenkins.Spec.Master.Image) == 0 {
		logger.Info("Setting default Jenkins master image: " + constants.DefaultJenkinsMasterImage)
		changed = true
		jenkins.Spec.Master.Image = constants.DefaultJenkinsMasterImage
	}
	if len(jenkins.Spec.Master.OperatorPlugins) == 0 {
		logger.Info("Setting default base plugins")
		changed = true
		jenkins.Spec.Master.OperatorPlugins = plugins.BasePlugins()
	}
	if len(jenkins.Spec.Master.Plugins) == 0 {
		changed = true
		jenkins.Spec.Master.Plugins = map[string][]string{"simple-theme-plugin:0.5.1": {}}
	}
	_, requestCPUSet := jenkins.Spec.Master.Resources.Requests[corev1.ResourceCPU]
	_, requestMemporySet := jenkins.Spec.Master.Resources.Requests[corev1.ResourceMemory]
	_, limitCPUSet := jenkins.Spec.Master.Resources.Limits[corev1.ResourceCPU]
	_, limitMemporySet := jenkins.Spec.Master.Resources.Limits[corev1.ResourceMemory]
	if !limitCPUSet || !limitMemporySet || !requestCPUSet || !requestMemporySet {
		logger.Info("Setting default Jenkins master pod resource requirements")
		changed = true
		jenkins.Spec.Master.Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("500Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1500m"),
				corev1.ResourceMemory: resource.MustParse("3Gi"),
			},
		}
	}

	if changed {
		return errors.WithStack(r.client.Update(context.TODO(), jenkins))
	}
	return nil
}
