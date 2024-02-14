/*


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

package hostedcluster

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/go-logr/logr"
	"github.com/openshift/hypershift/api/v1beta1"
	utils "github.com/openshift/route-monitor-operator/pkg/util"
	utilreconcile "github.com/openshift/route-monitor-operator/pkg/util/reconcile"
	"io"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"net/http"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"strings"
)

// HostedClusterReconciler reconciles a HostedCluster object
type HostedClusterReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}
type MonitorDetails struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

const annotationHTTPMonitorCreated = "hypershift.openshift.io/HTTP-monitor"
const annotationMonitorID = "hypershift.openshift.io/monitorID"
const apiURL = "https://hrm15629.live.dynatrace.com/api/v1/synthetic/monitors"

var DefaultConfigMap = "synthetic-monitor-configmap"

//+kubebuilder:rbac:groups=openshift.io,resources=hostedclusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=openshift.io,resources=hostedclusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=openshift.io,resources=hostedclusters/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the HostedCluster object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.4/pkg/reconcile

func (r *HostedClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithName("Reconcile").WithValues("name", req.Name, "namespace", req.Namespace)
	log.Info("Reconciling HostedCluster")

	// Fetch the HostedCluster instance
	hostedCluster := &v1beta1.HostedCluster{}
	err := r.Client.Get(ctx, req.NamespacedName, hostedCluster)
	if err != nil {
		log.Error(err, "unable to fetch HostedCluster")
		return utilreconcile.RequeueWith(err)
	}

	configMap, err := utils.GetOperatorConfigMap(r.Client, req.Namespace, DefaultConfigMap)
	if err != nil {
		log.Error(err, "Failed retrieving configmap")
		return reconcile.Result{}, err
	}

	// Check if the HostedCluster is marked for deletion
	if hostedCluster.DeletionTimestamp != nil {
		err = DeleteHTTPMonitor(hostedCluster, "")
		if err != nil {
			log.Error(err, "error deleting HTTP monitor")
			return utilreconcile.RequeueWith(err)
		}
		fmt.Println("Synthetic monitor deleted successfully.")

	}

	// Check if the HostedCluster is ready
	ready := IsHostedClusterReady(hostedCluster)

	if ready && strings.Contains(string(hostedCluster.Spec.Platform.AWS.EndpointAccess), "Public") {
		// Check if HTTP monitor has already been created
		if hostedCluster.Annotations[annotationHTTPMonitorCreated] == "" {
			// TODO add logic to grab dynatrace url
			monitorID, err := CreateHTTPMonitor(apiURL, "", hostedCluster, configMap)
			if err != nil {
				log.Error(err, "error creating HTTP monitor")
				return utilreconcile.RequeueWith(err)
			}

			//TODO add finilizer so HTTP moniter delete logic has a chance to run before hsoted cluster is deleted

			// Annotate HostedCluster to indicate it has an HTTP monitor
			hostedCluster.Annotations[annotationMonitorID] = monitorID
			hostedCluster.Annotations[annotationHTTPMonitorCreated] = "true"
			if err = r.Client.Update(ctx, hostedCluster); err != nil {
				log.Error(err, "error updating HostedCluster annotations")
				return utilreconcile.RequeueWith(err)
			}
		}
	} else {
		log.Info("HostedCluster not ready")
		return utilreconcile.RequeueWith(err)
	}

	return ctrl.Result{}, err
}

// IsHostedClusterReady checks if a HostedCluster is ready
func IsHostedClusterReady(hostedCluster *v1beta1.HostedCluster) bool {
	if hostedCluster.Status.ControlPlaneEndpoint.Host != "" {
		return true
	}
	return false
}

// / CreateHTTPMonitor creates a synthetic HTTP monitor
func CreateHTTPMonitor(apiURL, accessToken string, hostedCluster *v1beta1.HostedCluster, configMap *corev1.ConfigMap) (string, error) {
	// Construct payload for the synthetic monitor
	payload := map[string]interface{}{
		"name":                 configMap.Data["name"], // TODO unique name
		"enabled":              configMap.Data["enabled"],
		"frequencyMin":         configMap.Data["frequencyMin"],
		"locations":            hostedCluster.Spec.Platform.AWS.Region, //configMap.Data["locations"], TODO setup region mapping
		"manuallyAssignedApps": configMap.Data["manuallyAssignedApps"],
		"script":               configMap.Data["script"],
		"tags":                 configMap.Data["tags"], // TODO specifies that is managed by RMO & clusterid
		"type":                 configMap.Data["type"],
	}

	// Convert monitor details to JSON
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("error marshalling payload: %v", err)
	}

	// Create the HTTP request
	req, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(payloadBytes))
	if err != nil {
		return "", fmt.Errorf("error creating HTTP request: %v", err)
	}

	// Set headers and authentication
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)

	// Make the HTTP request
	client := http.DefaultClient
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("error making HTTP request: %v", err)
	}
	defer resp.Body.Close()

	// Read the response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("error reading response body: %v", err)
	}

	// Check the response status code
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status code: %v", resp.StatusCode)
	}

	fmt.Printf("Creating HTTP monitor for HostedCluster: %s\n", hostedCluster.Name)

	var monitorDetails MonitorDetails
	if err := json.Unmarshal(body, &monitorDetails); err != nil {
		return "", fmt.Errorf("error parsing monitor creation response: %v", err)
	}

	monitorID := monitorDetails.ID

	return monitorID, nil
}

// DeleteHTTPMonitor deletes a Dynatrace HTTP monitor
func DeleteHTTPMonitor(hostedCluster *v1beta1.HostedCluster, accessToken string) error {

	if hostedCluster.Annotations[annotationHTTPMonitorCreated] == "true" {
		req, err := http.NewRequest("DELETE", apiURL+"/"+hostedCluster.Annotations[annotationMonitorID], nil)
		if err != nil {
			return fmt.Errorf("error creating HTTP request: %v", err)
		}

		// Set headers and authentication
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+accessToken)

		// Make the HTTP request
		client := http.DefaultClient
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("error making HTTP request: %v", err)
		}
		defer resp.Body.Close()

		// Check the response status code
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("unexpected status code: %v", resp.StatusCode)
		}

	}
	fmt.Printf("Deleting Dynatrace HTTP monitor for HostedCluster %s in namespace %s\n", hostedCluster.Name, hostedCluster.Namespace)
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *HostedClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1beta1.HostedCluster{}).
		Complete(r)
}
