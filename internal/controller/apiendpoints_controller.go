/*
Copyright 2023.

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

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mitchellh/hashstructure/v2"
	log "github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	krakendv1 "github.com/nais/krakend/api/v1"
	"github.com/nais/krakend/internal/krakend"
	"github.com/nais/krakend/internal/netpol"
)

// ApiEndpointsReconciler reconciles a ApiEndpoints object
type ApiEndpointsReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	SyncInterval  time.Duration
	NetpolEnabled bool
	ClusterDomain string
}

const (
	AppLabelName        = "app"
	KrakendFinalizer    = "finalizer.krakend.nais.io"
	KrakendConfigMapKey = "endpoints.tmpl"
)

//+kubebuilder:rbac:groups=krakend.nais.io,resources=apiendpoints,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=krakend.nais.io,resources=apiendpoints/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=krakend.nais.io,resources=apiendpoints/finalizers,verbs=update

func (r *ApiEndpointsReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log.WithFields(log.Fields{
		"apiendpoints_name":      req.Name,
		"apiendpoints_namespace": req.Namespace,
	}).Infof("Reconciling ApiEndpoints")

	endpoints := &krakendv1.ApiEndpoints{}
	if err := r.Get(ctx, req.NamespacedName, endpoints); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	krakendName := endpoints.Spec.Krakend
	if krakendName == "" {
		krakendName = endpoints.Namespace
	}

	if endpoints.GetDeletionTimestamp() != nil {
		log.Debugf("Resource %s is marked for deletion", endpoints.Name)

		k := &krakendv1.Krakend{}
		err := r.Get(ctx, types.NamespacedName{
			Name:      krakendName,
			Namespace: endpoints.Namespace,
		}, k)
		if err != nil {
			if !errors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
			log.Debugf("krakend '%s' not found, nothing to do but remove finalizers", krakendName)
		} else {
			err = r.updateKrakendConfigMap(ctx, k)
			if err != nil {
				return ctrl.Result{}, err
			}
		}

		if controllerutil.RemoveFinalizer(endpoints, KrakendFinalizer) {
			err := r.Update(ctx, endpoints)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to remove finalizer: %w", err)
			}
		}
		return ctrl.Result{}, nil
	}

	hash, err := hashEndpoints(endpoints.Spec)
	if err != nil {
		return ctrl.Result{}, err
	}

	// skip reconciliation if hash is unchanged and timestamp is within sync interval
	// reconciliation is triggered when status subresource is updated, so we need this check to avoid infinite loop
	if endpoints.Status.SynchronizationHash == hash && !r.needsSync(endpoints.Status.SynchronizationTimestamp.Time) {
		log.Debugf("skipping reconciliation of %q, hash %q is unchanged and changed within syncInterval window", endpoints.Name, hash)
		return ctrl.Result{}, nil
	} else {
		log.Debugf("reconciling: hash changed: %v, outside syncInterval window: %v", endpoints.Status.SynchronizationHash != hash, r.needsSync(endpoints.Status.SynchronizationTimestamp.Time))
	}

	k := &krakendv1.Krakend{}
	err = r.Get(ctx, types.NamespacedName{
		Name:      krakendName,
		Namespace: endpoints.Namespace,
	}, k)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get Krakend instance '%s': %v", krakendName, err)
	}
	err = r.updateKrakendConfigMap(ctx, k)
	if err != nil {
		log.Errorf("updating Krakend configmap: %v", err)
		return ctrl.Result{}, err
	}

	if r.NetpolEnabled {
		if err := r.ensureAppIngressNetpol(ctx, endpoints); err != nil {
			log.Errorf("creating/updating netpol: %v", err)
			return ctrl.Result{}, nil
		}
	}

	// refetch to avoid the issue "the object has been modified, please apply
	// your changes to the latest version and try again" which would re-trigger the reconciliation
	if err := r.Get(ctx, req.NamespacedName, endpoints); err != nil {
		log.Error(err, "refetching resource after update")
		return ctrl.Result{}, err
	}
	needsUpdate := controllerutil.AddFinalizer(endpoints, KrakendFinalizer)
	if endpoints.GetOwnerReferences() == nil {
		ownerRef := []metav1.OwnerReference{
			{
				APIVersion: k.APIVersion,
				Kind:       k.Kind,
				Name:       k.Name,
				UID:        k.UID,
			},
		}

		endpoints.SetOwnerReferences(ownerRef)
		needsUpdate = true
	}

	if needsUpdate {
		if err := r.Update(ctx, endpoints); err != nil {
			return ctrl.Result{}, err
		}
	}

	// refetch to avoid the issue "the object has been modified, please apply
	// your changes to the latest version and try again" which would re-trigger the reconciliation
	if err := r.Get(ctx, req.NamespacedName, endpoints); err != nil {
		log.Error(err, "refetching resource after update")
		return ctrl.Result{}, err
	}
	endpoints.Status.SynchronizationTimestamp = metav1.Now()
	endpoints.Status.SynchronizationHash = hash
	if err := r.Status().Update(ctx, endpoints); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ApiEndpointsReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&krakendv1.ApiEndpoints{}).
		Complete(r)
}

func (r *ApiEndpointsReconciler) ensureAppIngressNetpol(ctx context.Context, endpoints *krakendv1.ApiEndpoints) error {
	apps := r.appsInNamespace(endpoints)
	log.Debugf("ensuring ingress netpols for apps: %v", apps)
	for _, app := range apps {
		ownerRef := []metav1.OwnerReference{
			{
				APIVersion: endpoints.APIVersion,
				Kind:       endpoints.Kind,
				Name:       endpoints.Name,
				UID:        endpoints.UID,
			},
		}

		krakendName := endpoints.Spec.Krakend
		if krakendName == "" {
			krakendName = endpoints.Namespace
		}
		npName := fmt.Sprintf("%s-%s-%s", "allow", krakendName, app)

		np := &networkingv1.NetworkPolicy{}
		err := r.Get(ctx, types.NamespacedName{
			Name:      npName,
			Namespace: endpoints.Namespace,
		}, np)

		if client.IgnoreNotFound(err) != nil {
			return err
		}

		if errors.IsNotFound(err) {
			np = netpol.AppAllowKrakendIngressNetpol(npName, endpoints.Namespace, map[string]string{
				AppLabelName: app,
			})
			np.SetOwnerReferences(ownerRef)

			err := r.Create(ctx, np)
			if err != nil {
				return fmt.Errorf("create netpol: %v", err)
			}
			log.Debugf("created netpol %s", npName)
			continue
		}

		err = r.Update(ctx, np)
		if err != nil {
			return fmt.Errorf("update netpol: %v", err)
		}
		log.Debugf("updated netpol %s", npName)
		continue
	}
	return nil
}

func (r *ApiEndpointsReconciler) appsInNamespace(endpoints *krakendv1.ApiEndpoints) []string {
	// used to remove duplicates
	seen := make(map[string]bool)
	apps := make([]string, 0)
	for _, e := range endpoints.Spec.Endpoints {
		u, err := url.Parse(e.BackendHost)
		if err != nil {
			log.Warnf("failed to parse backend host %s in ApiEndpoints %s, skipping: %v", e.BackendHost, endpoints.Name, err)
			continue
		}
		// only support http for service discovery
		if u.Scheme == "http" && u.Hostname() != "" {
			parts := strings.Split(u.Hostname(), ".")
			app := ""

			//e.g. http://app1 or http://app1.ns1 or http://app1.ns1.svc.cluster.local
			switch num := len(parts); {
			case num == 1:
				app = parts[0]
			case num == 2:
				if parts[1] == endpoints.Namespace {
					app = parts[0]
				}
			case num > 3:
				rest := strings.Join(parts[2:], ".")
				if parts[1] == endpoints.Namespace && rest == fmt.Sprintf("svc.%s", r.ClusterDomain) {
					app = parts[0]
				}
			}
			// only add app if not already seen
			if _, ok := seen[app]; !ok && app != "" {
				seen[app] = true
				apps = append(apps, app)
			}
		}
	}
	return apps
}

func (r *ApiEndpointsReconciler) updateKrakendConfigMap(ctx context.Context, k *krakendv1.Krakend) error {
	log.Debugf("updating ConfigMap for Krakend '%s'", k.Name)

	cm := &corev1.ConfigMap{}
	cmName := fmt.Sprintf("%s-%s-%s", k.Name, "krakend", "partials")
	err := r.Get(ctx, types.NamespacedName{
		Name:      cmName,
		Namespace: k.Namespace,
	}, cm)
	if err != nil {
		return fmt.Errorf("get ConfigMap '%s': %v", cmName, err)
	}

	isOwned := isConfigMapOwned(cm, k)
	if !isOwned {
		return fmt.Errorf("configmap '%s/%s' is not owned by Krakend '%s/%s'", cm.Namespace, cm.Name, k.Namespace, k.Name)
	}

	key := KrakendConfigMapKey
	ep := cm.Data[key]
	if ep == "" {
		return fmt.Errorf("%s not found in ConfigMap with name %s", key, cmName)
	}

	list := &krakendv1.ApiEndpointsList{}
	if err = r.List(ctx, list, client.InNamespace(k.Namespace)); err != nil {
		return fmt.Errorf("list all ApiEndpoints: %v", err)
	}

	filtered := make([]krakendv1.ApiEndpoints, 0)
	for _, e := range list.Items {
		if e.GetDeletionTimestamp() == nil {
			filtered = append(filtered, e)
		}
	}

	allEndpoints, err := krakend.ToKrakendEndpoints(k, filtered)
	if err != nil {
		return fmt.Errorf("convert ApiEndpoints to Krakend endpoints: %v", err)
	}
	partials, err := json.Marshal(allEndpoints)
	if err != nil {
		return err
	}

	//TODO handle race conditions when updating configmap
	cm.Data[key] = string(partials)
	err = r.Update(ctx, cm)
	if err != nil {
		return fmt.Errorf("update ConfigMap '%s': %v", cmName, err)
	}

	err = r.Get(ctx, types.NamespacedName{
		Name:      cmName,
		Namespace: k.Namespace,
	}, cm)
	if err != nil {
		return fmt.Errorf("get ConfigMap '%s': %v", cmName, err)
	}
	cmBytes, err := yaml.Marshal(cm)
	if err != nil {
		return fmt.Errorf("marshaling ConfigMap '%s': %v", cmName, err)
	}

	cmHash := sha256.Sum256(cmBytes)

	err = r.setCmHashToDeploymentAnnotations(ctx, k, cmHash)
	if err != nil {
		return fmt.Errorf("set cm hash to Deployment annotations: %v", err)
	}

	return nil
}

func (r ApiEndpointsReconciler) setCmHashToDeploymentAnnotations(ctx context.Context, k *krakendv1.Krakend, cmHash [32]byte) error {
	d := &appsv1.Deployment{}
	dName := fmt.Sprintf("%s-%s", k.Name, "krakend")
	err := r.Get(ctx, types.NamespacedName{
		Name:      dName,
		Namespace: k.Namespace,
	}, d)
	if client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("get Deployment '%s': %v", dName, err)
	}
	if errors.IsNotFound(err) {
		return nil
	}

	annotations := d.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations["checksum/cm-partials"] = fmt.Sprintf("%x", cmHash)
	d.SetAnnotations(annotations)

	err = r.Update(ctx, d)
	if err != nil {
		return fmt.Errorf("update Deployment '%s': %v", dName, err)
	}

	return nil
}

func isConfigMapOwned(cm *corev1.ConfigMap, k *krakendv1.Krakend) bool {
	refs := cm.GetOwnerReferences()
	for _, ref := range refs {
		if ref.APIVersion == k.APIVersion && ref.Kind == k.Kind && ref.Name == k.GetName() && ref.UID == k.GetUID() {
			return true
		}
	}

	return false
}

func hashEndpoints(a krakendv1.ApiEndpointsSpec) (string, error) {
	hash, err := hashstructure.Hash(a, hashstructure.FormatV2, nil)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", hash), nil
}

func (r *ApiEndpointsReconciler) needsSync(timestamp time.Time) bool {
	window := time.Now().Add(-r.SyncInterval)
	return timestamp.Before(window)
}
