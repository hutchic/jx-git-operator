package job

import (
	"fmt"
	"path/filepath"

	"github.com/jenkins-x/jx-git-operator/pkg/constants"
	"github.com/jenkins-x/jx-git-operator/pkg/launcher"
	"github.com/jenkins-x/jx-helpers/pkg/cmdrunner"
	"github.com/jenkins-x/jx-helpers/pkg/files"
	"github.com/jenkins-x/jx-helpers/pkg/kube/naming"
	"github.com/jenkins-x/jx-helpers/pkg/yamls"
	"github.com/jenkins-x/jx-kube-client/pkg/kubeclient"
	"github.com/jenkins-x/jx-logging/pkg/log"
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/runtime"

	v1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	v12 "k8s.io/client-go/kubernetes/typed/batch/v1"
)

type client struct {
	kubeClient kubernetes.Interface
	ns         string
	selector   string
	runner     cmdrunner.CommandRunner
}

// NewLauncher creates a new launcher for Jobs using the given kubernetes client and namespace
// if nil is passed in the kubernetes client will be lazily created
func NewLauncher(kubeClient kubernetes.Interface, ns string, selector string, runner cmdrunner.CommandRunner) (launcher.Interface, error) {
	if kubeClient == nil {
		f := kubeclient.NewFactory()
		cfg, err := f.CreateKubeConfig()
		if err != nil {
			return nil, errors.Wrapf(err, "failed to create kube config")
		}

		kubeClient, err = kubernetes.NewForConfig(cfg)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to create the kube client")
		}

		if ns == "" {
			ns, err = kubeclient.CurrentNamespace()
			if err != nil {
				return nil, errors.Wrapf(err, "failed to find the current namespace")
			}
		}
	}
	if runner == nil {
		runner = cmdrunner.DefaultCommandRunner
	}
	return &client{
		kubeClient: kubeClient,
		ns:         ns,
		selector:   selector,
		runner:     runner,
	}, nil
}

// Launch launches a job for the given commit
func (c *client) Launch(opts launcher.LaunchOptions) ([]runtime.Object, error) {
	ns := opts.Repository.Namespace
	if ns == "" {
		ns = c.ns
	}
	safeName := naming.ToValidValue(opts.Repository.Name)
	safeSha := naming.ToValidValue(opts.GitSHA)
	selector := fmt.Sprintf("%s,%s=%s", c.selector, launcher.RepositoryLabelKey, safeName)
	jobInterface := c.kubeClient.BatchV1().Jobs(ns)
	list, err := jobInterface.List(metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil && apierrors.IsNotFound(err) {
		err = nil
	}
	if err != nil {
		return nil, errors.Wrapf(err, "failed to find Jobs in namespace %s with selector %s", ns, selector)
	}

	var jobsForSha []v1.Job
	var activeJobs []v1.Job
	for _, r := range list.Items {
		log.Logger().Infof("found Job %s", r.Name)

		if r.Labels[launcher.CommitShaLabelKey] == safeSha {
			jobsForSha = append(jobsForSha, r)
		}

		// is the job active
		if IsJobActive(r) {
			activeJobs = append(activeJobs, r)
		}
	}

	if len(jobsForSha) == 0 {
		if len(activeJobs) > 0 {
			log.Logger().Infof("not creating a Job in namespace %s for repo %s sha %s yet as there is an active job %s", ns, safeName, safeSha, activeJobs[0].Name)
			return nil, nil
		}
		return c.startNewJob(opts, jobInterface, ns, safeName, safeSha)
	}
	return nil, nil
}

// IsJobActive returns true if the job has not completed or terminated yet
func IsJobActive(r v1.Job) bool {
	return r.Status.Succeeded == 0 && r.Status.Failed == 0
}

// startNewJob lets create a new Job resource
func (c *client) startNewJob(opts launcher.LaunchOptions, jobInterface v12.JobInterface, ns string, safeName string, safeSha string) ([]runtime.Object, error) {
	log.Logger().Infof("about to create a new job for name %s and sha %s", safeName, safeSha)

	// lets see if we are using a version stream to store the git operator configuration
	folder := filepath.Join(opts.Dir, "versionStream", "git-operator")
	exists, err := files.DirExists(folder)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to check if folder exists %s", folder)
	}
	if !exists {
		// lets try the original location
		folder = filepath.Join(opts.Dir, ".jx", "git-operator")
	}

	fileName := filepath.Join(folder, "job.yaml")
	exists, err = files.FileExists(fileName)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to find file %s in repository %s", fileName, safeName)
	}
	if !exists {
		return nil, errors.Errorf("repository %s does not have a Job file: %s", safeName, fileName)
	}

	resource := &v1.Job{}
	err = yamls.LoadFile(fileName, resource)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to load Job file %s in repository %s", fileName, safeName)
	}

	if !opts.NoResourceApply {
		// now lets check if there is a resources dir
		resourcesDir := filepath.Join(folder, "resources")
		exists, err = files.DirExists(resourcesDir)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to check if resources directory %s exists in repository %s", resourcesDir, safeName)
		}
		if exists {
			absDir, err := filepath.Abs(resourcesDir)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to get absolute resources dir %s", resourcesDir)
			}

			cmd := &cmdrunner.Command{
				Name: "kubectl",
				Args: []string{"apply", "-f", absDir},
			}
			log.Logger().Infof("running command: %s", cmd.CLI())
			_, err = c.runner(cmd)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to apply resources in dir %s", absDir)
			}
		}
	}

	// lets try use a maximum of 31 characters and a minimum of 10 for the sha
	namePrefix := trimLength(safeName, 20)
	maxShaLen := 30 - len(namePrefix)

	resourceName := namePrefix + "-" + trimLength(safeSha, maxShaLen)
	resource.Name = resourceName

	if resource.Labels == nil {
		resource.Labels = map[string]string{}
	}
	resource.Labels[constants.DefaultSelectorKey] = constants.DefaultSelectorValue
	resource.Labels[launcher.RepositoryLabelKey] = safeName
	resource.Labels[launcher.CommitShaLabelKey] = safeSha

	r2, err := jobInterface.Create(resource)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create Job %s in namespace %s", resourceName, ns)
	}
	log.Logger().Infof("created Job %s in namespace %s", resourceName, ns)
	return []runtime.Object{r2}, nil
}

func trimLength(text string, length int) string {
	if len(text) <= length {
		return text
	}
	return text[0:length]
}
