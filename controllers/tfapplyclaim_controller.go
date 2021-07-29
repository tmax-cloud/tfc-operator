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

// TFApplyClaimReconciler reconciles a TFApplyClaim object
type TFApplyClaimReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

var capacity int = 5
var commitID string

// +kubebuilder:rbac:groups=claim.tmax.io,resources=tfapplyclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=claim.tmax.io,resources=tfapplyclaims/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=claim.tmax.io,resources=tfapplyclaims/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods/exec,verbs=create

func (r *TFApplyClaimReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	log := r.Log.WithValues("tfapplyclaim", req.NamespacedName)

	// Fetch the "TFApplyClaim" instance
	apply := &claimv1alpha1.TFApplyClaim{}
	err := r.Get(ctx, req.NamespacedName, apply)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			log.Info("TFApplyClaim resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		log.Error(err, "Failed to get TFApplyClaim")
		return ctrl.Result{}, err
	}

	// your logic here
	repoType := apply.Spec.Type
	//version := apply.Spec.Version

	//versions := strings.Split(apply.Spec.Version, ".")

	//majorVersion, _ := strconv.Atoi(versions[0])
	//minorVersion, _ := strconv.Atoi(versions[1])
	//hotfixVersion := versions[2]

	url := apply.Spec.URL
	branch := apply.Spec.Branch

	//dest := "HCL_DIR"
	//opt_terraform := "-chdir=/" + dest // only terrform 0.14+

	secretName := apply.Spec.Secret

	secret := &corev1.Secret{}

	fmt.Println(repoType)
	fmt.Println(url)
	fmt.Println(branch)

	tfc_worker := os.Getenv("TFC_WORKER")
	fmt.Println(tfc_worker)

	helper, _ := patch.NewHelper(apply, r.Client)

	defer func() {
		if err := helper.Patch(ctx, apply); err != nil {
			log.Error(err, "TFApplyClaim patch error")
			///
			apply.Status.PrePhase = apply.Status.Phase
			apply.Status.Phase = "Error"
			apply.Status.Action = ""
			apply.Status.Reason = "TFApplyClaim patch error"

			err := r.Status().Update(ctx, apply)
			if err != nil {
				log.Error(err, "Failed to update TFApplyClaim status")
			}
			///
		}
	}()

	// Check the Secret (Git Credential) for Terraform HCL Code
	if repoType == "private" {
		if secretName == "" {
			apply.Status.PrePhase = ""
			apply.Status.Phase = "Error"
			apply.Status.Action = ""
			apply.Status.Reason = "Secret (git credential) is Needed"
			return ctrl.Result{}, err
		}

		err = r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: apply.Namespace}, secret)
		if err != nil {
			// Error reading the object - requeue the request.
			log.Error(err, "Failed to get Secret")
			apply.Status.PrePhase = ""
			apply.Status.Phase = "Error"
			apply.Status.Action = ""
			apply.Status.Reason = "Failed to get Secret"
			return ctrl.Result{}, err
		}

		_, exists_id := secret.Data["id"]
		_, exists_pw := secret.Data["pw"]

		if !exists_id && !exists_pw {
			apply.Status.PrePhase = ""
			apply.Status.Phase = "Error"
			apply.Status.Action = ""
			apply.Status.Reason = "Invalid Secret (id, pw)"
			return ctrl.Result{}, err
		}

		if apply.Status.Phase == "Error" && (apply.Status.Reason == "Secret (git credential) is Needed" ||
			apply.Status.Reason == "Failed to get Secret" || apply.Status.Reason == "Invalid Secret (id, pw)") {
			apply.Status.PrePhase = apply.Status.Phase
			apply.Status.Phase = "Awaiting"
			apply.Status.Reason = ""
		}
	}

	if apply.Status.Phase == "" || ((apply.Status.Phase == "Error" || apply.Status.Phase == "Rejected") && apply.Status.Action == "Approve") {
		apply.Status.PrePhase = apply.Status.Phase
		apply.Status.Phase = "Awaiting"
		return ctrl.Result{Requeue: true}, nil
	}

	if apply.Status.Action == "Reject" {
		apply.Status.PrePhase = apply.Status.Phase
		apply.Status.Phase = "Rejected"
	}

	if apply.Status.Phase != "Awaiting" || (apply.Status.Phase == "Awaiting" && apply.Status.Action == "Approve") {
		// Check if the deployment already exists, if not create a new one
		found := &appsv1.Deployment{}
		err = r.Get(ctx, types.NamespacedName{Name: apply.Name, Namespace: apply.Namespace}, found)
		if err != nil && errors.IsNotFound(err) {
			// Define a new deployment
			dep := r.deploymentForApply(apply)
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
		if (apply.Status.Phase == "Applied" || apply.Status.Phase == "Destroyed" || apply.Status.Phase == "Rejected") && apply.Spec.Destroy == false {
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
			apply.Status.Action = ""
			return ctrl.Result{}, nil
		}

		fmt.Println("15 seconds delay....")
		time.Sleep(time.Second * 15)

		// Update the Provider status with the pod names
		// List the pods for this provider's deployment
		podList := &corev1.PodList{}
		listOpts := []client.ListOption{
			client.InNamespace(apply.Namespace),
			client.MatchingLabels(labelsForApply(apply.Name)),
			client.MatchingFields{"status.phase": "Running"},
		}
		if err = r.List(ctx, podList, listOpts...); err != nil {
			log.Error(err, "Failed to list pods", "TFApplyClaim.Namespace", apply.Namespace, "TFApplyClaim.Name", apply.Name)
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
			apply.Status.PrePhase = apply.Status.Phase
			apply.Status.Phase = "Error"
			apply.Status.Action = ""
			apply.Status.Reason = "Failed to create in-cluster config"
			return ctrl.Result{}, err
		}
		// creates the clientset
		clientset, err := kubernetes.NewForConfig(config)
		if err != nil {
			log.Error(err, "Failed to create clientset")
			apply.Status.PrePhase = apply.Status.Phase
			apply.Status.Phase = "Error"
			apply.Status.Action = ""
			apply.Status.Reason = "Failed to create clientset"
			return ctrl.Result{}, err
		}

		// Go Client - POD EXEC
		if apply.Status.Phase == "Awaiting" && apply.Status.Action == "Approve" {

			// 1. Git Clone Repository
			/*
				var protocol string

				if strings.Contains(url, "http://") {
					protocol = "http://"
				} else if strings.Contains(url, "https://") {
					protocol = "https://"
				}

				url = strings.TrimLeft(url, protocol)

				if repoType == "private" {
					url = protocol + "$GIT_ID:$GIT_PW" + "@" + url
				} else {
					url = protocol + "TMP_ID:TMP_PW" + "@" + url
				}
			*/
			stdout.Reset()
			stderr.Reset()

			//rep_id := "GIT_ID=$(echo $GIT_ID | sed -e 's/\\!/%21/g' -e 's/\\#/%23/g' -e 's/\\$/%24/g' -e 's/\\&/%26/g' -e \"s/'/%27/g\" -e 's/(/%28/g' -e 's/)/%29/g' -e 's/\\*/%2A/g'  -e 's/\\+/%2B/g' -e 's/\\,/%2C/g' -e 's/\\//%2F/g' -e 's/\\:/%3A/g' -e 's/\\;/%3B/g' -e 's/\\=/%3D/g' -e 's/\\?/%3F/g' -e 's/\\@/%40/g' -e 's/\\[/%5B/g'  -e 's/\\]/%5D/g');"
			//rep_pw := "GIT_PW=$(echo $GIT_PW | sed -e 's/\\!/%21/g' -e 's/\\#/%23/g' -e 's/\\$/%24/g' -e 's/\\&/%26/g' -e \"s/'/%27/g\" -e 's/(/%28/g' -e 's/)/%29/g' -e 's/\\*/%2A/g'  -e 's/\\+/%2B/g' -e 's/\\,/%2C/g' -e 's/\\//%2F/g' -e 's/\\:/%3A/g' -e 's/\\;/%3B/g' -e 's/\\=/%3D/g' -e 's/\\?/%3F/g' -e 's/\\@/%40/g' -e 's/\\[/%5B/g'  -e 's/\\]/%5D/g');"

			//cmd := rep_id + rep_pw + "git clone " + url + " " + dest
			//err = util.ExecPodCmd(clientset, config, podNames[0], apply.Namespace, cmd, nil, &stdout, &stderr)

			//var cmd string

			err = util.ExecClone(clientset, config, podNames[0], apply.Namespace, nil, &stdout, &stderr, apply)

			fmt.Println(stdout.String())
			fmt.Println(stderr.String())

			if err != nil && !strings.Contains(stdout.String(), "already exists") {
				log.Error(err, "Failed to Clone Git Repository")
				apply.Status.PrePhase = apply.Status.Phase
				apply.Status.Phase = "Error"
				apply.Status.Action = ""
				apply.Status.Reason = "Failed to Clone Git Repository"
				return ctrl.Result{}, err
			}

			if apply.Spec.Branch != "" {
				stdout.Reset()
				stderr.Reset()

				//cmd = "cd " + dest + ";" + "git checkout -t origin/" + apply.Spec.Branch
				//err = util.ExecPodCmd(clientset, config, podNames[0], apply.Namespace, cmd, nil, &stdout, &stderr)
				err = util.ExecBranchCheckout(clientset, config, podNames[0], apply.Namespace, nil, &stdout, &stderr, apply)

				fmt.Println(stdout.String())
				fmt.Println(stderr.String())

				if err != nil && !strings.Contains(stdout.String(), "already exists") {
					log.Error(err, "Failed to Checkout Git Branch")
					apply.Status.PrePhase = apply.Status.Phase
					apply.Status.Phase = "Error"
					apply.Status.Action = ""
					apply.Status.Reason = "Failed to Checkout Git Branch"
					return ctrl.Result{}, err
				}
			}

			// 2. Terraform Initialization
			stdout.Reset()
			stderr.Reset()
			/*
				cmd = "cd /tmp;" +
					fmt.Sprintf("wget %s/%s/%s_%s_linux_amd64.zip;", util.TERRAFORM_BINARY_URL, version, util.TERRAFORM_BINARY_NAME, version) +
					fmt.Sprintf("wget %s/%s/%s_%s_SHA256SUMS;", util.TERRAFORM_BINARY_URL, version, util.TERRAFORM_BINARY_NAME, version) +
					fmt.Sprintf("unzip -o -d /bin %s_%s_linux_amd64.zip;", util.TERRAFORM_BINARY_NAME, version) +
					"rm -rf /tmp/build;"

				err = util.ExecPodCmd(clientset, config, podNames[0], apply.Namespace, cmd, nil, &stdout, &stderr)
			*/
			err = util.ExecTerraformDownload(clientset, config, podNames[0], apply.Namespace, nil, &stdout, &stderr, apply)

			fmt.Println(stdout.String())
			fmt.Println(stderr.String())

			if err != nil {
				log.Error(err, "Failed to Download Terraform")
				apply.Status.PrePhase = apply.Status.Phase
				apply.Status.Phase = "Error"
				apply.Status.Action = ""
				apply.Status.Reason = "Failed to Download Terraform"
				return ctrl.Result{}, err
			}

			stdout.Reset()
			stderr.Reset()

			//cmd := "terraform init" + " " + opt_terraform
			/*
				if int(majorVersion) >= 1 || minorVersion >= 15 {
					cmd = "cd " + dest + ";" + "terraform init"
				} else {
					cmd = "cd " + dest + ";" + "terraform init -verify-plugins=false"
				}
				err = util.ExecPodCmd(clientset, config, podNames[0], apply.Namespace, cmd, nil, &stdout, &stderr)
			*/
			err = util.ExecTerraformInit(clientset, config, podNames[0], apply.Namespace, nil, &stdout, &stderr, apply)

			fmt.Println(stdout.String())
			fmt.Println(stderr.String())

			if err != nil {
				log.Error(err, "Failed to Initialize Terraform")
				apply.Status.PrePhase = apply.Status.Phase
				apply.Status.Phase = "Error"
				apply.Status.Action = ""
				apply.Status.Reason = "Failed to Initialize Terraform"
				return ctrl.Result{}, err
			} else {
				apply.Status.PrePhase = apply.Status.Phase
				apply.Status.Phase = "Approved"
			}
		}
		//}

		// 3. Terraform Plan
		//if (apply.Status.Phase == "Ready" || apply.Status.Phase == "Planned") && apply.Status.Action == "Plan" {
		if (apply.Status.Phase == "Approved" || apply.Status.Phase == "Planned") && apply.Status.Action == "Plan" {
			// Git Pull
			stdout.Reset()
			stderr.Reset()
			/*
				cmd := "cd " + dest + ";" + "git pull"
				err = util.ExecPodCmd(clientset, config, podNames[0], apply.Namespace, cmd, nil, &stdout, &stderr)
			*/
			err = util.ExecGitPull(clientset, config, podNames[0], apply.Namespace, nil, &stdout, &stderr, apply)

			fmt.Println(stdout.String())
			fmt.Println(stderr.String())

			if err != nil {
				log.Error(err, "Failed to Pull Git Repository")
				apply.Status.PrePhase = apply.Status.Phase
				apply.Status.Phase = "Error"
				apply.Status.Action = ""
				apply.Status.Reason = "Failed to Pull Git Repository"
				return ctrl.Result{}, err
			}

			// Get Commit ID
			stdout.Reset()
			stderr.Reset()

			/*
				cmd = "cd " + dest + ";" +
					"git log --pretty=format:\"%H\" | head -n 1"

				err = util.ExecPodCmd(clientset, config, podNames[0], apply.Namespace, cmd, nil, &stdout, &stderr)
			*/
			err = util.ExecGetCommitID(clientset, config, podNames[0], apply.Namespace, nil, &stdout, &stderr, apply)

			fmt.Println(stdout.String())

			if err != nil {
				log.Error(err, "Failed to Get Commit ID")
				apply.Status.PrePhase = apply.Status.Phase
				apply.Status.Phase = "Error"
				apply.Status.Action = ""
				apply.Status.Reason = "Failed to Get Commit ID"
				return ctrl.Result{}, err
			} else {
				commitID = strings.TrimRight(stdout.String(), "\r\n")
			}
			/*
				if int(majorVersion) >= 1 || minorVersion >= 15 {
					cmd = "cd " + dest + ";" + "terraform init"
				} else {
					cmd = "cd " + dest + ";" + "terraform init -verify-plugins=false"
				}
				err = util.ExecPodCmd(clientset, config, podNames[0], apply.Namespace, cmd, nil, &stdout, &stderr)
			*/
			err = util.ExecTerraformInit(clientset, config, podNames[0], apply.Namespace, nil, &stdout, &stderr, apply)

			fmt.Println(stdout.String())
			fmt.Println(stderr.String())

			if err != nil {
				log.Error(err, "Failed to Initialize Terraform")
				apply.Status.PrePhase = apply.Status.Phase
				apply.Status.Phase = "Error"
				apply.Status.Action = ""
				apply.Status.Reason = "Failed to Initialize Terraform"
				return ctrl.Result{}, err
			}

			if apply.Spec.Variable != "" {
				stdout.Reset()
				stderr.Reset()
				/*
					cmd = "cat > terraform.tfvars.json << EOL\n" +
						apply.Spec.Variable + "\nEOL"
					err = util.ExecPodCmd(clientset, config, podNames[0], apply.Namespace, cmd, nil, &stdout, &stderr)
				*/
				err = util.ExecCreateVariables(clientset, config, podNames[0], apply.Namespace, nil, &stdout, &stderr, apply)

				fmt.Println(stdout.String())
				fmt.Println(stderr.String())

				if err != nil {
					log.Error(err, "Failed to Create Variable Definitions (.tfvars) Files")
					apply.Status.PrePhase = apply.Status.Phase
					apply.Status.Phase = "Error"
					apply.Status.Action = ""
					apply.Status.Reason = "Failed to Create Variable Definitions (.tfvars) Files"
					return ctrl.Result{}, err
				}
			}

			stdout.Reset()
			stderr.Reset()

			//cmd := "terraform init" + " " + opt_terraform
			/*
				cmd = "cd " + dest + ";" + "terraform plan"
				err = util.ExecPodCmd(clientset, config, podNames[0], apply.Namespace, cmd, nil, &stdout, &stderr)
			*/
			err = util.ExecTerraformPlan(clientset, config, podNames[0], apply.Namespace, nil, &stdout, &stderr, apply)

			fmt.Println(stdout.String())
			fmt.Println(stderr.String())

			stdoutStderr := stdout.String() + "\n" + stderr.String()

			//apply.Status.PrePhase = apply.Status.Phase
			//apply.Status.Phase = "Planned"
			// add plan to plans
			var plan claimv1alpha1.Plan
			//plan.LastExectionTime = time.Now().String()
			plan.LastExectionTime = time.Now().Format("2006-01-02 15:04:05") // yyyy-MM-dd HH:mm:ss
			plan.Commit = commitID
			plan.Log = stdoutStderr

			if len(apply.Status.Plans) == capacity {
				apply.Status.Plans = dequeuePlan(apply.Status.Plans, capacity)
			}
			apply.Status.Plans = append([]claimv1alpha1.Plan{plan}, apply.Status.Plans...)

			if err != nil {
				log.Error(err, "Failed to Plan Terraform")
				apply.Status.PrePhase = apply.Status.Phase
				apply.Status.Phase = "Error"
				apply.Status.Action = ""
				apply.Status.Reason = "Failed to Plan Terraform"
				return ctrl.Result{}, err
			} else {
				apply.Status.PrePhase = apply.Status.Phase
				apply.Status.Phase = "Planned"
			}

		}

		// 4. Terraform Apply
		if (apply.Status.Phase == "Approved" || apply.Status.Phase == "Planned") && apply.Status.Action == "Apply" {
			// Get Commit ID
			stdout.Reset()
			stderr.Reset()

			/*
				cmd := "cd " + dest + ";" +
					"git log --pretty=format:\"%H\" | head -n 1"

				err = util.ExecPodCmd(clientset, config, podNames[0], apply.Namespace, cmd, nil, &stdout, &stderr)
			*/
			err = util.ExecGetCommitID(clientset, config, podNames[0], apply.Namespace, nil, &stdout, &stderr, apply)
			fmt.Println(stdout.String())

			if err != nil {
				log.Error(err, "Failed to Get Commit ID")
				apply.Status.PrePhase = apply.Status.Phase
				apply.Status.Phase = "Error"
				apply.Status.Action = ""
				apply.Status.Reason = "Failed to Get Commit ID"
				return ctrl.Result{}, err
			} else {
				apply.Status.Commit = strings.TrimRight(stdout.String(), "\r\n")
				apply.Status.URL = apply.Spec.URL
				apply.Status.Branch = apply.Spec.Branch
			}

			if apply.Spec.Variable != "" {
				stdout.Reset()
				stderr.Reset()
				/*
					cmd := "cat > terraform.tfvars.json << EOL\n" +
						apply.Spec.Variable + "\nEOL"
					err = util.ExecPodCmd(clientset, config, podNames[0], apply.Namespace, cmd, nil, &stdout, &stderr)
				*/
				err = util.ExecCreateVariables(clientset, config, podNames[0], apply.Namespace, nil, &stdout, &stderr, apply)

				fmt.Println(stdout.String())
				fmt.Println(stderr.String())

				if err != nil {
					log.Error(err, "Failed to Create Variable Definitions (.tfvars) Files")
					apply.Status.PrePhase = apply.Status.Phase
					apply.Status.Phase = "Error"
					apply.Status.Action = ""
					apply.Status.Reason = "Failed to Create Variable Definitions (.tfvars) Files"
					return ctrl.Result{}, err
				}
			}

			stdout.Reset()
			stderr.Reset()
			/*
				cmd = "cd " + dest + ";" + "terraform apply -auto-approve"
				err = util.ExecPodCmd(clientset, config, podNames[0], apply.Namespace, cmd, nil, &stdout, &stderr)
			*/
			err = util.ExecTerraformApply(clientset, config, podNames[0], apply.Namespace, nil, &stdout, &stderr, apply)

			fmt.Println(stdout.String())
			fmt.Println(stderr.String())

			stdoutStderr := stdout.String() + "\n" + stderr.String()

			apply.Status.Apply = stdoutStderr

			if err != nil {
				log.Error(err, "Failed to Apply Terraform")
				apply.Status.PrePhase = apply.Status.Phase
				apply.Status.Phase = "Error"
				apply.Status.Action = ""
				apply.Status.Reason = "Failed to Apply Terraform"
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
			/*
				cmd = "cd " + dest + ";" +
					"cat terraform.tfstate"

				err = util.ExecPodCmd(clientset, config, podNames[0], apply.Namespace, cmd, nil, &stdout, &stderr)
			*/
			err = util.ExecReadState(clientset, config, podNames[0], apply.Namespace, nil, &stdout, &stderr, apply)
			fmt.Println(stdout.String())

			if err != nil {
				log.Error(err, "Failed to Read tfstate file")
				apply.Status.PrePhase = apply.Status.Phase
				apply.Status.Phase = "Error"
				apply.Status.Action = ""
				apply.Status.Reason = "Failed to Read tfstate file"
				return ctrl.Result{}, err
			} else {
				apply.Status.State = stdout.String()
				apply.Status.Resource.Added = added
				apply.Status.Resource.Updated = changed
				apply.Status.Resource.Deleted = destroyed

				apply.Status.PrePhase = apply.Status.Phase
				apply.Status.Phase = "Applied"
			}
		}

		// 5. Terraform Destroy (if required)
		if apply.Status.Phase == "Applied" && apply.Spec.Destroy == true {
			/*
				var protocol string

				if strings.Contains(url, "http://") {
					protocol = "http://"
				} else if strings.Contains(url, "https://") {
					protocol = "https://"
				}

				url = strings.TrimLeft(url, protocol)

				if repoType == "private" {
					url = protocol + "$GIT_ID:$GIT_PW" + "@" + url
				} else {
					url = protocol + "TMP_ID:TMP_PW" + "@" + url
				}
			*/
			stdout.Reset()
			stderr.Reset()

			//rep_id := "GIT_ID=$(echo $GIT_ID | sed -e 's/\\!/%21/g' -e 's/\\#/%23/g' -e 's/\\$/%24/g' -e 's/\\&/%26/g' -e \"s/'/%27/g\" -e 's/(/%28/g' -e 's/)/%29/g' -e 's/\\*/%2A/g'  -e 's/\\+/%2B/g' -e 's/\\,/%2C/g' -e 's/\\//%2F/g' -e 's/\\:/%3A/g' -e 's/\\;/%3B/g' -e 's/\\=/%3D/g' -e 's/\\?/%3F/g' -e 's/\\@/%40/g' -e 's/\\[/%5B/g'  -e 's/\\]/%5D/g');"
			//rep_pw := "GIT_PW=$(echo $GIT_PW | sed -e 's/\\!/%21/g' -e 's/\\#/%23/g' -e 's/\\$/%24/g' -e 's/\\&/%26/g' -e \"s/'/%27/g\" -e 's/(/%28/g' -e 's/)/%29/g' -e 's/\\*/%2A/g'  -e 's/\\+/%2B/g' -e 's/\\,/%2C/g' -e 's/\\//%2F/g' -e 's/\\:/%3A/g' -e 's/\\;/%3B/g' -e 's/\\=/%3D/g' -e 's/\\?/%3F/g' -e 's/\\@/%40/g' -e 's/\\[/%5B/g'  -e 's/\\]/%5D/g');"
			/*
				cmd := rep_id + rep_pw + "git clone " + url + " " + dest
				err = util.ExecPodCmd(clientset, config, podNames[0], apply.Namespace, cmd, nil, &stdout, &stderr)
			*/
			err = util.ExecClone(clientset, config, podNames[0], apply.Namespace, nil, &stdout, &stderr, apply)

			fmt.Println(stdout.String())
			fmt.Println(stderr.String())

			if err != nil && !strings.Contains(stdout.String(), "already exists") {
				log.Error(err, "Failed to Clone Git Repository")
				apply.Status.PrePhase = apply.Status.Phase
				apply.Status.Phase = "Error"
				apply.Status.Action = ""
				apply.Status.Reason = "Failed to Clone Git Repository"
				return ctrl.Result{}, err
			}

			if apply.Spec.Branch != "" {
				stdout.Reset()
				stderr.Reset()
				/*
					cmd = "cd " + dest + ";" + "git checkout -t origin/" + apply.Spec.Branch
					err = util.ExecPodCmd(clientset, config, podNames[0], apply.Namespace, cmd, nil, &stdout, &stderr)
				*/
				err = util.ExecBranchCheckout(clientset, config, podNames[0], apply.Namespace, nil, &stdout, &stderr, apply)
				fmt.Println(stdout.String())
				fmt.Println(stderr.String())

				if err != nil && !strings.Contains(stdout.String(), "already exists") {
					log.Error(err, "Failed to Checkout Git Branch")
					apply.Status.PrePhase = apply.Status.Phase
					apply.Status.Phase = "Error"
					apply.Status.Action = ""
					apply.Status.Reason = "Failed to Checkout Git Branch"
					return ctrl.Result{}, err
				}
			}

			stdout.Reset()
			stderr.Reset()
			/*
				cmd = "cd /tmp;" +
					fmt.Sprintf("wget %s/%s/%s_%s_linux_amd64.zip;", util.TERRAFORM_BINARY_URL, version, util.TERRAFORM_BINARY_NAME, version) +
					fmt.Sprintf("wget %s/%s/%s_%s_SHA256SUMS;", util.TERRAFORM_BINARY_URL, version, util.TERRAFORM_BINARY_NAME, version) +
					fmt.Sprintf("unzip -o -d /bin %s_%s_linux_amd64.zip;", util.TERRAFORM_BINARY_NAME, version) +
					"rm -rf /tmp/build;"

				err = util.ExecPodCmd(clientset, config, podNames[0], apply.Namespace, cmd, nil, &stdout, &stderr)

				fmt.Println(stdout.String())
				fmt.Println(stderr.String())
			*/
			err = util.ExecTerraformDownload(clientset, config, podNames[0], apply.Namespace, nil, &stdout, &stderr, apply)

			if err != nil {
				log.Error(err, "Failed to Initialize Terraform")
				apply.Status.PrePhase = apply.Status.Phase
				apply.Status.Phase = "Error"
				apply.Status.Action = ""
				apply.Status.Reason = "Failed to Initialize Terraform"
				return ctrl.Result{}, err
			}

			stdout.Reset()
			stderr.Reset()
			/*
				if int(majorVersion) >= 1 || minorVersion >= 15 {
					cmd = "cd " + dest + ";" + "terraform init"
				} else {
					cmd = "cd " + dest + ";" + "terraform init -verify-plugins=false"
				}
				err = util.ExecPodCmd(clientset, config, podNames[0], apply.Namespace, cmd, nil, &stdout, &stderr)
			*/
			err = util.ExecTerraformInit(clientset, config, podNames[0], apply.Namespace, nil, &stdout, &stderr, apply)

			fmt.Println(stdout.String())
			fmt.Println(stderr.String())

			if err != nil {
				log.Error(err, "Failed to Initialize Terraform")
				apply.Status.PrePhase = apply.Status.Phase
				apply.Status.Phase = "Error"
				apply.Status.Action = ""
				apply.Status.Reason = "Failed to Initialize Terraform"
				return ctrl.Result{}, err
			}

			// Revert to Commit Point
			stdout.Reset()
			stderr.Reset()
			/*
				cmd = "cd " + dest + ";" +
					"git reset " + apply.Status.Commit

				err = util.ExecPodCmd(clientset, config, podNames[0], apply.Namespace, cmd, nil, &stdout, &stderr)
			*/
			err = util.ExecRevertCommit(clientset, config, podNames[0], apply.Namespace, nil, &stdout, &stderr, apply)
			fmt.Println(stdout.String())

			if err != nil {
				log.Error(err, "Failed to Revert Commit")
				apply.Status.PrePhase = apply.Status.Phase
				apply.Status.Phase = "Error"
				apply.Status.Action = ""
				apply.Status.Reason = "Failed to Revert Commit"
				return ctrl.Result{}, err
			}

			// Recover Terraform State
			stdout.Reset()
			stderr.Reset()
			/*
				cmd = "cd " + dest + ";" +
					"cat > terraform.tfstate << EOL\n" +
					apply.Status.State + "EOL"

				err = util.ExecPodCmd(clientset, config, podNames[0], apply.Namespace, cmd, nil, &stdout, &stderr)
			*/
			err = util.ExecRecoverState(clientset, config, podNames[0], apply.Namespace, nil, &stdout, &stderr, apply)
			fmt.Println(stdout.String())

			if err != nil {
				log.Error(err, "Failed to Recover Terraform State")
				apply.Status.PrePhase = apply.Status.Phase
				apply.Status.Phase = "Error"
				apply.Status.Action = ""
				apply.Status.Reason = "Failed to Recover Terraform State"
				return ctrl.Result{}, err
			}

			if apply.Spec.Variable != "" {
				stdout.Reset()
				stderr.Reset()
				/*
					cmd := "cat > terraform.tfvars.json << EOL\n" +
						apply.Spec.Variable + "\nEOL"
					err = util.ExecPodCmd(clientset, config, podNames[0], apply.Namespace, cmd, nil, &stdout, &stderr)
				*/
				err = util.ExecCreateVariables(clientset, config, podNames[0], apply.Namespace, nil, &stdout, &stderr, apply)
				fmt.Println(stdout.String())
				fmt.Println(stderr.String())

				if err != nil {
					log.Error(err, "Failed to Create Variable Definitions (.tfvars) Files")
					apply.Status.PrePhase = apply.Status.Phase
					apply.Status.Phase = "Error"
					apply.Status.Action = ""
					apply.Status.Reason = "Failed to Create Variable Definitions (.tfvars) Files"
					return ctrl.Result{}, err
				}
			}

			stdout.Reset()
			stderr.Reset()
			/*
				cmd = "cd " + dest + ";" + "terraform destroy -auto-approve"
				err = util.ExecPodCmd(clientset, config, podNames[0], apply.Namespace, cmd, nil, &stdout, &stderr)
			*/
			err = util.ExecTerraformDestroy(clientset, config, podNames[0], apply.Namespace, nil, &stdout, &stderr, apply)
			fmt.Println(stdout.String())
			fmt.Println(stderr.String())

			stdoutStderr := stdout.String() + "\n" + stderr.String()

			apply.Status.Destroy = stdoutStderr

			if err != nil {
				log.Error(err, "Failed to Destroy Terraform")
				apply.Status.PrePhase = apply.Status.Phase
				apply.Status.Phase = "Error"
				apply.Status.Action = ""
				apply.Status.Reason = "Failed to Destroy Terraform"
				return ctrl.Result{}, err
			}

			var matched string
			var added, changed, destroyed int

			lines := strings.Split(string(stdoutStderr), "\n")

			for i, line := range lines {
				if strings.Contains(line, "Destroy complete!") {
					matched = lines[i]
					s := strings.Split(string(matched), " ")

					//added, _ = strconv.Atoi(s[3])
					//changed, _ = strconv.Atoi(s[5])
					//destroyed, _ = strconv.Atoi(s[7])
					// In Destroy
					destroyed, _ = strconv.Atoi(s[3])
				}
			}

			stdout.Reset()
			stderr.Reset()
			/*
				cmd = "cd " + dest + ";" +
					"cat terraform.tfstate"

				err = util.ExecPodCmd(clientset, config, podNames[0], apply.Namespace, cmd, nil, &stdout, &stderr)
			*/
			err = util.ExecReadState(clientset, config, podNames[0], apply.Namespace, nil, &stdout, &stderr, apply)
			fmt.Println(stdout.String())

			if err != nil {
				log.Error(err, "Failed to Read tfstate file")
				apply.Status.PrePhase = apply.Status.Phase
				apply.Status.Phase = "Error"
				apply.Status.Reason = "Failed to Read tfstate file"
				return ctrl.Result{}, err
			} else {
				apply.Status.State = stdout.String()
				apply.Status.Resource.Added = added
				apply.Status.Resource.Updated = changed
				apply.Status.Resource.Deleted = destroyed

				apply.Spec.Destroy = false
				apply.Status.PrePhase = apply.Status.Phase
				apply.Status.Phase = "Destroyed"
			}

		}
	}

	apply.Status.Action = ""
	return ctrl.Result{RequeueAfter: time.Second * 60}, nil // Reconcile loop rescheduled after 60 seconds

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
									Name: "GIT_ID",
									ValueFrom: &corev1.EnvVarSource{
										SecretKeyRef: &corev1.SecretKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{Name: m.Spec.Secret},
											Key:                  "id",
										},
									},
								},
								{
									Name: "GIT_PW",
									ValueFrom: &corev1.EnvVarSource{
										SecretKeyRef: &corev1.SecretKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{Name: m.Spec.Secret},
											Key:                  "pw",
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

func (r *TFApplyClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {

	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Pod{}, "status.phase", func(rawObj runtime.Object) []string {
		pod := rawObj.(*corev1.Pod)
		return []string{string(pod.Status.DeepCopy().Phase)}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&claimv1alpha1.TFApplyClaim{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Pod{}).
		Complete(r)
}

func dequeuePlan(slice []v1alpha1.Plan, capacity int) []v1alpha1.Plan {
	fmt.Println(slice[1:])
	fmt.Println(slice[:capacity-1])
	return slice[:capacity-1]
}
