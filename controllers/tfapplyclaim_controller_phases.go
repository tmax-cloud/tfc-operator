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

package controllers

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"context"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/tmax-cloud/tfc-operator/api/v1alpha1"
	claimv1alpha1 "github.com/tmax-cloud/tfc-operator/api/v1alpha1"
	"github.com/tmax-cloud/tfc-operator/util"

	"os"
)



var capacity int = 5
var commitID string

func (r *TFApplyClaimReconciler) ReadyClaim(ctx context.Context, tfapplyclaim *claimv1alpha1.TFApplyClaim) (ctrl.Result, error) {
	repoType := tfapplyclaim.Spec.Type

	// Check the Secret (Git Credential) for Terraform HCL Code
	if repoType == "private" {
		if secretName == "" {
			tfapplyclaim.Status.PrePhase = ""
			tfapplyclaim.Status.Phase = "Error"
			tfapplyclaim.Status.Action = ""
			tfapplyclaim.Status.Reason = "Secret (git credential) is Needed"
			return ctrl.Result{}, err
		}

		err = r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: tfapplyclaim.Namespace}, secret)
		if err != nil {
			// Error reading the object - requeue the request.
			log.Error(err, "Failed to get Secret")
			tfapplyclaim.Status.PrePhase = ""
			tfapplyclaim.Status.Phase = "Error"
			tfapplyclaim.Status.Action = ""
			tfapplyclaim.Status.Reason = "Failed to get Secret"
			return ctrl.Result{}, err
		}

		_, exists_token := secret.Data["token"]

		if !exists_token {
			tfapplyclaim.Status.PrePhase = ""
			tfapplyclaim.Status.Phase = "Error"
			tfapplyclaim.Status.Action = ""
			tfapplyclaim.Status.Reason = "Invalid Secret (token)"
			return ctrl.Result{}, err
		}

		if tfapplyclaim.Status.Phase == "Error" && (tfapplyclaim.Status.Reason == "Secret (git credential) is Needed" ||
			tfapplyclaim.Status.Reason == "Failed to get Secret" || tfapplyclaim.Status.Reason == "Invalid Secret (token)") {
			tfapplyclaim.Status.PrePhase = tfapplyclaim.Status.Phase
			tfapplyclaim.Status.Phase = "Awaiting"
			tfapplyclaim.Status.Reason = ""
		}
	}
}

func (r *TFApplyClaimReconciler) ApproveClaim(ctx context.Context, tfapplyclaim *claimv1alpha1.TFApplyClaim) (ctrl.Result, error) {
	// Check if the deployment already exists, if not create a new one
	found := &appsv1.Deployment{}
	err = r.Get(ctx, types.NamespacedName{Name: tfapplyclaim.Name, Namespace: tfapplyclaim.Namespace}, found)
	if err != nil && errors.IsNotFound(err) {
		// Define a new deployment
		dep := r.deploymentForApply(tfapplyclaim)
		log.Info("Creating a new Deployment", "Deployment.Namespace", dep.Namespace, "Deployment.Name", dep.Name)
		err = r.Create(ctx, dep)
		if err != nil {
			log.Error(err, "Failed to create new Deployment", "Deployment.Namespace", dep.Namespace, "Deployment.Name", dep.Name)
			return ctrl.Result{}, err
		}
		// Deployment created successfully - return and requeue
		return ctrl.Result{Requeue: true}, nil
	} else if err != nil {
		log.Error(err, "Failed to get Deployment")
		return ctrl.Result{}, err
	}

	// Ensure the deployment size is the same as the spec
	size := int32(1)
	if (tfapplyclaim.Status.Phase == "Applied" || tfapplyclaim.Status.Phase == "Destroyed" || tfapplyclaim.Status.Phase == "Rejected") && tfapplyclaim.Spec.Destroy == false {
		size = 0
	}
	if *found.Spec.Replicas != size {
		found.Spec.Replicas = &size
		err = r.Update(ctx, found)
		if err != nil {
			log.Error(err, "Failed to update Deployment", "Deployment.Namespace", found.Namespace, "Deployment.Name", found.Name)
			return ctrl.Result{}, err
		}
		// Spec updated - return and requeue
		return ctrl.Result{Requeue: true}, nil
	}

	if size == 0 {
		log.Info("There's no need to Create Terraform Pod...")
		tfapplyclaim.Status.Action = ""
		return ctrl.Result{}, nil
	}

	fmt.Println("15 seconds delay....")
	time.Sleep(time.Second * 15)

	// Update the Provider status with the pod names
	// List the pods for this provider's deployment
	podList := &corev1.PodList{}
	listOpts := []client.ListOption{
		client.InNamespace(tfapplyclaim.Namespace),
		client.MatchingLabels(labelsForApply(tfapplyclaim.Name)),
		client.MatchingFields{"status.phase": "Running"},
	}
	if err = r.List(ctx, podList, listOpts...); err != nil {
		log.Error(err, "Failed to list pods", "TFApplyClaim.Namespace", tfapplyclaim.Namespace, "TFApplyClaim.Name", tfapplyclaim.Name)
		return ctrl.Result{}, err
	}
	podNames := getPodNames(podList.Items)

	if len(podNames) < 1 {
		log.Info("Not yet create Terraform Pod...")
		return ctrl.Result{RequeueAfter: time.Second * 5}, nil
	} else if len(podNames) > 1 {
		log.Info("Not yet terminate Previous Terraform Pod...")
		return ctrl.Result{RequeueAfter: time.Second * 5}, nil
	} else {
		log.Info("Ready to Execute Terraform Pod!")
	}

	fmt.Println(podNames)
	fmt.Println("podNames[0]:" + podNames[0])

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	// creates the in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Error(err, "Failed to create in-cluster config")
		tfapplyclaim.Status.PrePhase = tfapplyclaim.Status.Phase
		tfapplyclaim.Status.Phase = "Error"
		tfapplyclaim.Status.Action = ""
		tfapplyclaim.Status.Reason = "Failed to create in-cluster config"
		return ctrl.Result{}, err
	}
	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Error(err, "Failed to create clientset")
		tfapplyclaim.Status.PrePhase = tfapplyclaim.Status.Phase
		tfapplyclaim.Status.Phase = "Error"
		tfapplyclaim.Status.Action = ""
		tfapplyclaim.Status.Reason = "Failed to create clientset"
		return ctrl.Result{}, err
	}

	// Go Client - POD EXEC
	if tfapplyclaim.Status.Phase == "Awaiting" && tfapplyclaim.Status.Action == "Approve" {

		// 1. Git Clone Repository
		stdout.Reset()
		stderr.Reset()

		err = util.ExecClone(clientset, config, podNames[0], tfapplyclaim.Namespace, nil, &stdout, &stderr, tfapplyclaim)

		fmt.Println(stdout.String())
		fmt.Println(stderr.String())

		if err != nil && !strings.Contains(stdout.String(), "already exists") {
			log.Error(err, "Failed to Clone Git Repository")
			tfapplyclaim.Status.PrePhase = tfapplyclaim.Status.Phase
			tfapplyclaim.Status.Phase = "Error"
			tfapplyclaim.Status.Action = ""
			tfapplyclaim.Status.Reason = "Failed to Clone Git Repository"
			return ctrl.Result{}, err
		}

		if tfapplyclaim.Spec.Branch != "" {
			stdout.Reset()
			stderr.Reset()

			err = util.ExecBranchCheckout(clientset, config, podNames[0], tfapplyclaim.Namespace, nil, &stdout, &stderr, tfapplyclaim)

			fmt.Println(stdout.String())
			fmt.Println(stderr.String())

			if err != nil && !strings.Contains(stdout.String(), "already exists") {
				log.Error(err, "Failed to Checkout Git Branch")
				tfapplyclaim.Status.PrePhase = tfapplyclaim.Status.Phase
				tfapplyclaim.Status.Phase = "Error"
				tfapplyclaim.Status.Action = ""
				tfapplyclaim.Status.Reason = "Failed to Checkout Git Branch"
				return ctrl.Result{}, err
			}
		}

		// 2. Terraform Initialization
		stdout.Reset()
		stderr.Reset()

		err = util.ExecTerraformDownload(clientset, config, podNames[0], tfapplyclaim.Namespace, nil, &stdout, &stderr, tfapplyclaim)

		fmt.Println(stdout.String())
		fmt.Println(stderr.String())

		if err != nil {
			log.Error(err, "Failed to Download Terraform")
			tfapplyclaim.Status.PrePhase = tfapplyclaim.Status.Phase
			tfapplyclaim.Status.Phase = "Error"
			tfapplyclaim.Status.Action = ""
			tfapplyclaim.Status.Reason = "Failed to Download Terraform"
			return ctrl.Result{}, err
		}

		stdout.Reset()
		stderr.Reset()

		err = util.ExecTerraformInit(clientset, config, podNames[0], tfapplyclaim.Namespace, nil, &stdout, &stderr, tfapplyclaim)

		fmt.Println(stdout.String())
		fmt.Println(stderr.String())

		if err != nil {
			log.Error(err, "Failed to Initialize Terraform")
			tfapplyclaim.Status.PrePhase = tfapplyclaim.Status.Phase
			tfapplyclaim.Status.Phase = "Error"
			tfapplyclaim.Status.Action = ""
			tfapplyclaim.Status.Reason = "Failed to Initialize Terraform"
			return ctrl.Result{}, err
		} else {
			tfapplyclaim.Status.PrePhase = tfapplyclaim.Status.Phase
			tfapplyclaim.Status.Phase = "Approved"
		}
	}
}


func (r *TFApplyClaimReconciler) PlanClaim(ctx context.Context, tfapplyclaim *claimv1alpha1.TFApplyClaim) (ctrl.Result, error) {
	// 3. Terraform Plan
	if (tfapplyclaim.Status.Phase == "Approved" || tfapplyclaim.Status.Phase == "Planned") && tfapplyclaim.Status.Action == "Plan" {
		// Git Pull
		stdout.Reset()
		stderr.Reset()

		err = util.ExecGitPull(clientset, config, podNames[0], tfapplyclaim.Namespace, nil, &stdout, &stderr, tfapplyclaim)

		fmt.Println(stdout.String())
		fmt.Println(stderr.String())

		if err != nil {
			log.Error(err, "Failed to Pull Git Repository")
			tfapplyclaim.Status.PrePhase = tfapplyclaim.Status.Phase
			tfapplyclaim.Status.Phase = "Error"
			tfapplyclaim.Status.Action = ""
			tfapplyclaim.Status.Reason = "Failed to Pull Git Repository"
			return ctrl.Result{}, err
		}

		// Get Commit ID
		stdout.Reset()
		stderr.Reset()

		err = util.ExecGetCommitID(clientset, config, podNames[0], tfapplyclaim.Namespace, nil, &stdout, &stderr, tfapplyclaim)

		fmt.Println(stdout.String())

		if err != nil {
			log.Error(err, "Failed to Get Commit ID")
			tfapplyclaim.Status.PrePhase = tfapplyclaim.Status.Phase
			tfapplyclaim.Status.Phase = "Error"
			tfapplyclaim.Status.Action = ""
			tfapplyclaim.Status.Reason = "Failed to Get Commit ID"
			return ctrl.Result{}, err
		} else {
			commitID = strings.TrimRight(stdout.String(), "\r\n")
		}

		err = util.ExecTerraformInit(clientset, config, podNames[0], tfapplyclaim.Namespace, nil, &stdout, &stderr, tfapplyclaim)

		fmt.Println(stdout.String())
		fmt.Println(stderr.String())

		if err != nil {
			log.Error(err, "Failed to Initialize Terraform")
			tfapplyclaim.Status.PrePhase = tfapplyclaim.Status.Phase
			tfapplyclaim.Status.Phase = "Error"
			tfapplyclaim.Status.Action = ""
			tfapplyclaim.Status.Reason = "Failed to Initialize Terraform"
			return ctrl.Result{}, err
		}

		if tfapplyclaim.Spec.Variable != "" {
			stdout.Reset()
			stderr.Reset()

			err = util.ExecCreateVariables(clientset, config, podNames[0], tfapplyclaim.Namespace, nil, &stdout, &stderr, tfapplyclaim)

			fmt.Println(stdout.String())
			fmt.Println(stderr.String())

			if err != nil {
				log.Error(err, "Failed to Create Variable Definitions (.tfvars) Files")
				tfapplyclaim.Status.PrePhase = tfapplyclaim.Status.Phase
				tfapplyclaim.Status.Phase = "Error"
				tfapplyclaim.Status.Action = ""
				tfapplyclaim.Status.Reason = "Failed to Create Variable Definitions (.tfvars) Files"
				return ctrl.Result{}, err
			}
		}

		stdout.Reset()
		stderr.Reset()

		err = util.ExecTerraformPlan(clientset, config, podNames[0], tfapplyclaim.Namespace, nil, &stdout, &stderr, tfapplyclaim)

		fmt.Println(stdout.String())
		fmt.Println(stderr.String())

		stdoutStderr := stdout.String() + "\n" + stderr.String()


		// add plan to plans
		var plan claimv1alpha1.Plan

		plan.LastExectionTime = time.Now().Format("2006-01-02 15:04:05") // yyyy-MM-dd HH:mm:ss
		plan.Commit = commitID
		plan.Log = stdoutStderr

		if len(tfapplyclaim.Status.Plans) == capacity {
			tfapplyclaim.Status.Plans = dequeuePlan(tfapplyclaim.Status.Plans, capacity)
		}
		tfapplyclaim.Status.Plans = append([]claimv1alpha1.Plan{plan}, tfapplyclaim.Status.Plans...)

		if err != nil {
			log.Error(err, "Failed to Plan Terraform")
			tfapplyclaim.Status.PrePhase = tfapplyclaim.Status.Phase
			tfapplyclaim.Status.Phase = "Error"
			tfapplyclaim.Status.Action = ""
			tfapplyclaim.Status.Reason = "Failed to Plan Terraform"
			return ctrl.Result{}, err
		} else {
			tfapplyclaim.Status.PrePhase = tfapplyclaim.Status.Phase
			tfapplyclaim.Status.Phase = "Planned"
		}

	}
}

func (r *TFApplyClaimReconciler) ApplyClaim(ctx context.Context, tfapplyclaim *claimv1alpha1.TFApplyClaim) (ctrl.Result, error) {
	// 4. Terraform Apply
	if (tfapplyclaim.Status.Phase == "Approved" || tfapplyclaim.Status.Phase == "Planned") && tfapplyclaim.Status.Action == "Apply" {
		// Get Commit ID
		stdout.Reset()
		stderr.Reset()

		err = util.ExecGetCommitID(clientset, config, podNames[0], tfapplyclaim.Namespace, nil, &stdout, &stderr, tfapplyclaim)
		fmt.Println(stdout.String())

		if err != nil {
			log.Error(err, "Failed to Get Commit ID")
			tfapplyclaim.Status.PrePhase = tfapplyclaim.Status.Phase
			tfapplyclaim.Status.Phase = "Error"
			tfapplyclaim.Status.Action = ""
			tfapplyclaim.Status.Reason = "Failed to Get Commit ID"
			return ctrl.Result{}, err
		} else {
			tfapplyclaim.Status.Commit = strings.TrimRight(stdout.String(), "\r\n")
			tfapplyclaim.Status.URL = tfapplyclaim.Spec.URL
			tfapplyclaim.Status.Branch = tfapplyclaim.Spec.Branch
		}

		if tfapplyclaim.Spec.Variable != "" {
			stdout.Reset()
			stderr.Reset()

			err = util.ExecCreateVariables(clientset, config, podNames[0], tfapplyclaim.Namespace, nil, &stdout, &stderr, tfapplyclaim)

			fmt.Println(stdout.String())
			fmt.Println(stderr.String())

			if err != nil {
				log.Error(err, "Failed to Create Variable Definitions (.tfvars) Files")
				tfapplyclaim.Status.PrePhase = tfapplyclaim.Status.Phase
				tfapplyclaim.Status.Phase = "Error"
				tfapplyclaim.Status.Action = ""
				tfapplyclaim.Status.Reason = "Failed to Create Variable Definitions (.tfvars) Files"
				return ctrl.Result{}, err
			}
		}

		stdout.Reset()
		stderr.Reset()

		err = util.ExecTerraformApply(clientset, config, podNames[0], tfapplyclaim.Namespace, nil, &stdout, &stderr, tfapplyclaim)

		fmt.Println(stdout.String())
		fmt.Println(stderr.String())

		stdoutStderr := stdout.String() + "\n" + stderr.String()

		tfapplyclaim.Status.Apply = stdoutStderr

		if err != nil {
			log.Error(err, "Failed to Apply Terraform")
			tfapplyclaim.Status.PrePhase = tfapplyclaim.Status.Phase
			tfapplyclaim.Status.Phase = "Error"
			tfapplyclaim.Status.Action = ""
			tfapplyclaim.Status.Reason = "Failed to Apply Terraform"
			return ctrl.Result{}, err
		}

		var matched string
		var added, changed, destroyed int

		lines := strings.Split(string(stdoutStderr), "\n")

		for i, line := range lines {
			if strings.Contains(line, "Apply complete!") {
				matched = lines[i]
				s := strings.Split(string(matched), " ")

				added, _ = strconv.Atoi(s[3])
				changed, _ = strconv.Atoi(s[5])
				destroyed, _ = strconv.Atoi(s[7])
			}
		}

		stdout.Reset()
		stderr.Reset()

		// Read Terraform State File
		err = util.ExecReadState(clientset, config, podNames[0], tfapplyclaim.Namespace, nil, &stdout, &stderr, tfapplyclaim)
		fmt.Println(stdout.String())

		if err != nil {
			log.Error(err, "Failed to Read tfstate file")
			tfapplyclaim.Status.PrePhase = tfapplyclaim.Status.Phase
			tfapplyclaim.Status.Phase = "Error"
			tfapplyclaim.Status.Action = ""
			tfapplyclaim.Status.Reason = "Failed to Read tfstate file"
			return ctrl.Result{}, err
		} else {
			tfapplyclaim.Status.State = stdout.String()
			tfapplyclaim.Status.Resource.Added = added
			tfapplyclaim.Status.Resource.Updated = changed
			tfapplyclaim.Status.Resource.Deleted = destroyed

			tfapplyclaim.Status.PrePhase = tfapplyclaim.Status.Phase
			tfapplyclaim.Status.Phase = "Applied"

			// Add finalizer first if not exist to avoid the race condition between init and delete
			if !controllerutil.ContainsFinalizer(tfapplyclaim, "claim.tmax.io/terraform-protection") {
				controllerutil.AddFinalizer(tfapplyclaim, "claim.tmax.io/terraform-protection")
			}
		
		}
	}
}

func (r *TFApplyClaimReconciler) DestroyClaim(ctx context.Context, tfapplyclaim *claimv1alpha1.TFApplyClaim) (ctrl.Result, error) {
	// 5. Terraform Destroy (if required)
	if tfapplyclaim.Status.Phase == "Applied" && tfapplyclaim.Spec.Destroy == true {

		stdout.Reset()
		stderr.Reset()

		err = util.ExecClone(clientset, config, podNames[0], tfapplyclaim.Namespace, nil, &stdout, &stderr, tfapplyclaim)

		fmt.Println(stdout.String())
		fmt.Println(stderr.String())

		if err != nil && !strings.Contains(stdout.String(), "already exists") {
			log.Error(err, "Failed to Clone Git Repository")
			tfapplyclaim.Status.PrePhase = tfapplyclaim.Status.Phase
			tfapplyclaim.Status.Phase = "Error"
			tfapplyclaim.Status.Action = ""
			tfapplyclaim.Status.Reason = "Failed to Clone Git Repository"
			return ctrl.Result{}, err
		}

		if tfapplyclaim.Spec.Branch != "" {
			stdout.Reset()
			stderr.Reset()

			err = util.ExecBranchCheckout(clientset, config, podNames[0], tfapplyclaim.Namespace, nil, &stdout, &stderr, tfapplyclaim)
			fmt.Println(stdout.String())
			fmt.Println(stderr.String())

			if err != nil && !strings.Contains(stdout.String(), "already exists") {
				log.Error(err, "Failed to Checkout Git Branch")
				tfapplyclaim.Status.PrePhase = tfapplyclaim.Status.Phase
				tfapplyclaim.Status.Phase = "Error"
				tfapplyclaim.Status.Action = ""
				tfapplyclaim.Status.Reason = "Failed to Checkout Git Branch"
				return ctrl.Result{}, err
			}
		}

		stdout.Reset()
		stderr.Reset()

		err = util.ExecTerraformDownload(clientset, config, podNames[0], tfapplyclaim.Namespace, nil, &stdout, &stderr, tfapplyclaim)

		if err != nil {
			log.Error(err, "Failed to Initialize Terraform")
			tfapplyclaim.Status.PrePhase = tfapplyclaim.Status.Phase
			tfapplyclaim.Status.Phase = "Error"
			tfapplyclaim.Status.Action = ""
			tfapplyclaim.Status.Reason = "Failed to Initialize Terraform"
			return ctrl.Result{}, err
		}

		stdout.Reset()
		stderr.Reset()

		err = util.ExecTerraformInit(clientset, config, podNames[0], tfapplyclaim.Namespace, nil, &stdout, &stderr, tfapplyclaim)

		fmt.Println(stdout.String())
		fmt.Println(stderr.String())

		if err != nil {
			log.Error(err, "Failed to Initialize Terraform")
			tfapplyclaim.Status.PrePhase = tfapplyclaim.Status.Phase
			tfapplyclaim.Status.Phase = "Error"
			tfapplyclaim.Status.Action = ""
			tfapplyclaim.Status.Reason = "Failed to Initialize Terraform"
			return ctrl.Result{}, err
		}

		// Revert to Commit Point
		stdout.Reset()
		stderr.Reset()

		err = util.ExecRevertCommit(clientset, config, podNames[0], tfapplyclaim.Namespace, nil, &stdout, &stderr, tfapplyclaim)
		fmt.Println(stdout.String())

		if err != nil {
			log.Error(err, "Failed to Revert Commit")
			tfapplyclaim.Status.PrePhase = tfapplyclaim.Status.Phase
			tfapplyclaim.Status.Phase = "Error"
			tfapplyclaim.Status.Action = ""
			tfapplyclaim.Status.Reason = "Failed to Revert Commit"
			return ctrl.Result{}, err
		}

		// Recover Terraform State
		stdout.Reset()
		stderr.Reset()

		err = util.ExecRecoverState(clientset, config, podNames[0], tfapplyclaim.Namespace, nil, &stdout, &stderr, tfapplyclaim)
		fmt.Println(stdout.String())

		if err != nil {
			log.Error(err, "Failed to Recover Terraform State")
			tfapplyclaim.Status.PrePhase = tfapplyclaim.Status.Phase
			tfapplyclaim.Status.Phase = "Error"
			tfapplyclaim.Status.Action = ""
			tfapplyclaim.Status.Reason = "Failed to Recover Terraform State"
			return ctrl.Result{}, err
		}

		if tfapplyclaim.Spec.Variable != "" {
			stdout.Reset()
			stderr.Reset()

			err = util.ExecCreateVariables(clientset, config, podNames[0], tfapplyclaim.Namespace, nil, &stdout, &stderr, tfapplyclaim)
			fmt.Println(stdout.String())
			fmt.Println(stderr.String())

			if err != nil {
				log.Error(err, "Failed to Create Variable Definitions (.tfvars) Files")
				tfapplyclaim.Status.PrePhase = tfapplyclaim.Status.Phase
				tfapplyclaim.Status.Phase = "Error"
				tfapplyclaim.Status.Action = ""
				tfapplyclaim.Status.Reason = "Failed to Create Variable Definitions (.tfvars) Files"
				return ctrl.Result{}, err
			}
		}

		stdout.Reset()
		stderr.Reset()

		err = util.ExecTerraformDestroy(clientset, config, podNames[0], tfapplyclaim.Namespace, nil, &stdout, &stderr, tfapplyclaim)
		fmt.Println(stdout.String())
		fmt.Println(stderr.String())

		stdoutStderr := stdout.String() + "\n" + stderr.String()

		tfapplyclaim.Status.Destroy = stdoutStderr

		if err != nil {
			log.Error(err, "Failed to Destroy Terraform")
			tfapplyclaim.Status.PrePhase = tfapplyclaim.Status.Phase
			tfapplyclaim.Status.Phase = "Error"
			tfapplyclaim.Status.Action = ""
			tfapplyclaim.Status.Reason = "Failed to Destroy Terraform"
			return ctrl.Result{}, err
		}

		var matched string
		var added, changed, destroyed int

		lines := strings.Split(string(stdoutStderr), "\n")

		for i, line := range lines {
			if strings.Contains(line, "Destroy complete!") {
				matched = lines[i]
				s := strings.Split(string(matched), " ")

				destroyed, _ = strconv.Atoi(s[3])
			}
		}

		stdout.Reset()
		stderr.Reset()

		err = util.ExecReadState(clientset, config, podNames[0], tfapplyclaim.Namespace, nil, &stdout, &stderr, tfapplyclaim)
		fmt.Println(stdout.String())

		if err != nil {
			log.Error(err, "Failed to Read tfstate file")
			tfapplyclaim.Status.PrePhase = tfapplyclaim.Status.Phase
			tfapplyclaim.Status.Phase = "Error"
			tfapplyclaim.Status.Reason = "Failed to Read tfstate file"
			return ctrl.Result{}, err
		} else {
			tfapplyclaim.Status.State = stdout.String()
			tfapplyclaim.Status.Resource.Added = added
			tfapplyclaim.Status.Resource.Updated = changed
			tfapplyclaim.Status.Resource.Deleted = destroyed

			tfapplyclaim.Spec.Destroy = false
			tfapplyclaim.Status.PrePhase = tfapplyclaim.Status.Phase
			tfapplyclaim.Status.Phase = "Destroyed"

			controllerutil.RemoveFinalizer(tfapplyclaim, "claim.tmax.io/terraform-protection")
		}
	}
}

// deploymentForProvider returns a provider Deployment object
func (r *TFApplyClaimReconciler) deploymentForApply(m *claimv1alpha1.TFApplyClaim) *appsv1.Deployment {
	ls := labelsForApply(m.Name)
	replicas := int32(1) //m.Spec.Size
	image_path := os.Getenv("TFC_WORKER")

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.Name,
			Namespace: m.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: ls,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: ls,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Image:           image_path, //"tmaxcloudck/tfc-worker:v0.0.1",
						Name:            "ubuntu",
						Command:         []string{"/bin/sleep", "3650d"},
						ImagePullPolicy: "Always",
						Ports: []corev1.ContainerPort{{
							ContainerPort: 11211,
							Name:          "ubuntu",
						}},
					}},
				},
			},
		},
	}
	if m.Spec.Type == "private" {
		dep = &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      m.Name,
				Namespace: m.Namespace,
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{
					MatchLabels: ls,
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: ls,
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Image:           image_path, //"tmaxcloudck/tfc-worker:v0.0.1",
							Name:            "ubuntu",
							Command:         []string{"/bin/sleep", "3650d"},
							ImagePullPolicy: "Always",
							Ports: []corev1.ContainerPort{{
								ContainerPort: 11211,
								Name:          "ubuntu",
							}},
							Env: []corev1.EnvVar{
								{
									Name: "GIT_TOKEN",
									ValueFrom: &corev1.EnvVarSource{
										SecretKeyRef: &corev1.SecretKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{Name: m.Spec.Secret},
											Key:                  "token",
										},
									},
								},
							},
						}},
					},
				},
			},
		}
	}
	// Set Provider instance as the owner and controller
	ctrl.SetControllerReference(m, dep, r.Scheme)
	return dep
}

// labelsForProvider returns the labels for selecting the resources
// belonging to the given Provider CR name.
func labelsForApply(name string) map[string]string {
	return map[string]string{"app": "tfapplyclaim", "tfapplyclaim_cr": name}
}

// getPodNames returns the pod names of the array of pods passed in
func getPodNames(pods []corev1.Pod) []string {
	var podNames []string
	for _, pod := range pods {
		podNames = append(podNames, pod.Name)
	}
	return podNames
}


func dequeuePlan(slice []v1alpha1.Plan, capacity int) []v1alpha1.Plan {
	fmt.Println(slice[1:])
	fmt.Println(slice[:capacity-1])
	return slice[:capacity-1]
}
