package cmd

import (
	"fmt"
	"github.com/jenkins-x/jx/pkg/gits"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/jenkins-x/golang-jenkins"
	"github.com/jenkins-x/jx/pkg/apis/jenkins.io/v1"
	"github.com/jenkins-x/jx/pkg/builds"
	"github.com/jenkins-x/jx/pkg/client/clientset/versioned"
	"github.com/jenkins-x/jx/pkg/kube"
	"github.com/spf13/cobra"
	"gopkg.in/AlecAivazis/survey.v1/terminal"
	"k8s.io/client-go/kubernetes"

	"github.com/jenkins-x/jx/pkg/jx/cmd/templates"
	"github.com/jenkins-x/jx/pkg/log"
	"github.com/jenkins-x/jx/pkg/util"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GetBuildLogsOptions the command line options
type GetBuildLogsOptions struct {
	GetOptions

	Tail          bool
	Wait          bool
	BuildFilter   builds.BuildPodInfoFilter
	CurrentFolder bool
}

var (
	get_build_log_long = templates.LongDesc(`
		Display a build log

`)

	get_build_log_example = templates.Examples(`
		# Display a build log - with the user choosing which repo + build to view
		jx get build log

		# Pick a build to view the log based on the repo cheese
		jx get build log --repo cheese

		# Pick a pending knative build to view the log based 
		jx get build log -p

		# Pick a pending knative build to view the log based on the repo cheese
		jx get build log --repo cheese -p

		# Pick a knative build for the 1234 Pull Request on the repo cheese
		jx get build log --repo cheese --branch PR-1234

	`)
)

// NewCmdGetBuildLogs creates the command
func NewCmdGetBuildLogs(f Factory, in terminal.FileReader, out terminal.FileWriter, errOut io.Writer) *cobra.Command {
	options := &GetBuildLogsOptions{
		GetOptions: GetOptions{
			CommonOptions: CommonOptions{
				Factory: f,
				In:      in,

				Out: out,
				Err: errOut,
			},
		},
	}

	cmd := &cobra.Command{
		Use:     "log [flags]",
		Short:   "Display a build log",
		Long:    get_build_log_long,
		Example: get_build_log_example,
		Aliases: []string{"logs"},
		Run: func(cmd *cobra.Command, args []string) {
			options.Cmd = cmd
			options.Args = args
			err := options.Run()
			CheckErr(err)
		},
	}
	cmd.Flags().BoolVarP(&options.Tail, "tail", "t", true, "Tails the build log to the current terminal")
	cmd.Flags().BoolVarP(&options.Wait, "wait", "w", false, "Waits for the build to start before failing")
	cmd.Flags().BoolVarP(&options.BuildFilter.Pending, "pending", "p", false, "Only display logs which are currently pending to choose from if no build name is supplied")
	cmd.Flags().StringVarP(&options.BuildFilter.Filter, "filter", "f", "", "Filters all the available jobs by those that contain the given text")
	cmd.Flags().StringVarP(&options.BuildFilter.Owner, "owner", "o", "", "Filters the owner (person/organisation) of the repository")
	cmd.Flags().StringVarP(&options.BuildFilter.Repository, "repo", "r", "", "Filters the build repository")
	cmd.Flags().StringVarP(&options.BuildFilter.Branch, "branch", "", "", "Filters the branch")
	cmd.Flags().StringVarP(&options.BuildFilter.Build, "build", "b", "", "The build number to view")
	cmd.Flags().BoolVarP(&options.CurrentFolder, "current", "c", false, "Display logs using current folder as repo name, and parent folder as owner")

	return cmd
}

// Run implements this command
func (o *GetBuildLogsOptions) Run() error {
	jxClient, ns, err := o.JXClientAndDevNamespace()
	if err != nil {
		return err
	}
	kubeClient, err := o.KubeClient()
	if err != nil {
		return err
	}

	devEnv, err := kube.GetEnrichedDevEnvironment(kubeClient, jxClient, ns)
	webhookEngine := devEnv.Spec.WebHookEngine
	if webhookEngine == v1.WebHookEngineProw {
		return o.getProwBuildLog(kubeClient, jxClient, ns)
	}

	args := o.Args

	if !o.BatchMode && len(args) == 0 {
		jobMap, err := o.getJobMap(o.BuildFilter.Filter)
		if err != nil {
			return err
		}
		names := []string{}
		for k := range jobMap {
			names = append(names, k)
		}
		sort.Strings(names)
		if len(names) == 0 {
			return fmt.Errorf("No pipelines have been built!")
		}

		defaultName := ""
		for _, n := range names {
			if strings.HasSuffix(n, "/master") {
				defaultName = n
				break
			}
		}
		name, err := util.PickNameWithDefault(names, "Which pipeline do you want to view the logs of?: ", defaultName, "", o.In, o.Out, o.Err)
		if err != nil {
			return err
		}
		args = []string{name}
	}
	if len(args) == 0 {
		return fmt.Errorf("No pipeline chosen")
	}
	name := args[0]
	buildNumber := o.BuildFilter.BuildNumber()

	last, err := o.getLastJenkinsBuild(name, buildNumber)
	if err != nil {
		return err
	}

	log.Infof("%s %s\n", util.ColorStatus("view the log at:"), util.ColorInfo(util.UrlJoin(last.Url, "/console")))
	return o.tailBuild(name, &last)
}

func (o *GetBuildLogsOptions) getLastJenkinsBuild(name string, buildNumber int) (gojenkins.Build, error) {
	var last gojenkins.Build

	jenkinsClient, err := o.JenkinsClient()
	if err != nil {
		return last, err
	}

	f := func() error {
		var err error

		jobMap, err := o.getJobMap(o.BuildFilter.Filter)
		if err != nil {
			return err
		}
		job := jobMap[name]
		if job.Url == "" {
			return fmt.Errorf("No Job exists yet called %s", name)
		}

		if buildNumber > 0 {
			last, err = jenkinsClient.GetBuild(job, buildNumber)
		} else {
			last, err = jenkinsClient.GetLastBuild(job)
		}
		if err != nil {
			return err
		}
		if last.Url == "" {
			if buildNumber > 0 {
				return fmt.Errorf("No build found for name %s number %d", name, buildNumber)
			} else {
				return fmt.Errorf("No build found for name %s", name)
			}
		}
		return err
	}

	if o.Wait {
		err := o.retry(60, time.Second*2, f)
		return last, err
	} else {
		err := f()
		return last, err
	}
}

func (o *GetBuildLogsOptions) getProwBuildLog(kubeClient kubernetes.Interface, jxClient versioned.Interface, ns string) error {
	pods, err := builds.GetBuildPods(kubeClient, ns)
	if err != nil {
		log.Warnf("Failed to query pods %s\n", err)
		return err
	}

	if o.CurrentFolder {
		currentDirectory, err := os.Getwd()
		if err != nil {
			return err
		}

		gitRepository, err := gits.NewGitCLI().Info(currentDirectory)
		if err != nil {
			return err
		}

		o.BuildFilter.Repository = gitRepository.Name
		o.BuildFilter.Owner = gitRepository.Organisation
	}

	buildInfos := []*builds.BuildPodInfo{}
	for _, pod := range pods {
		initContainers := pod.Spec.InitContainers
		if len(initContainers) > 0 {
			buildInfo := builds.CreateBuildPodInfo(pod)
			if o.BuildFilter.BuildMatches(buildInfo) {
				buildInfos = append(buildInfos, buildInfo)
			}
		}
	}
	builds.SortBuildPodInfos(buildInfos)
	if len(buildInfos) == 0 {
		return fmt.Errorf("No knative builds have been triggered which match the current filter!")
	}

	args := o.Args
	names := []string{}
	buildMap := map[string]*builds.BuildPodInfo{}
	pipelineMap := map[string]*builds.BuildPodInfo{}

	defaultName := ""
	for _, build := range buildInfos {
		name := build.Pipeline + " #" + build.Build
		names = append(names, name)
		buildMap[name] = build
		pipelineMap[build.Pipeline] = build

		if build.Branch == "master" {
			defaultName = name
		}
	}

	if len(args) == 0 {
		name, err := util.PickNameWithDefault(names, "Which build do you want to view the logs of?: ", defaultName, "", o.In, o.Out, o.Err)
		if err != nil {
			return err
		}
		args = []string{name}
	}
	if len(args) == 0 {
		return fmt.Errorf("No pipeline chosen")
	}
	name := args[0]
	build := buildMap[name]
	suffix := ""
	if build == nil {
		build = pipelineMap[name]
		if build != nil {
			suffix = " #" + build.Build
		}
	}
	if build == nil {
		return fmt.Errorf("No Pipeline found for name %s in values: %s", name, strings.Join(names, ", "))
	}

	pod := build.Pod
	if pod == nil {
		return fmt.Errorf("No Pod found for name %s", name)
	}
	initContainers := pod.Spec.InitContainers
	if len(initContainers) <= 0 {
		return fmt.Errorf("No InitContainers for Pod %s for build: %s", pod.Name, name)
	}

	log.Infof("Build logs for %s\n", util.ColorInfo(name+suffix))
	err = waitForInitContainerToStart(kubeClient, ns, pod)
	if err != nil {
		return err
	}
	for _, ic := range initContainers {
		err = o.getPodLog(ns, pod, ic)
		if err != nil {
			return err
		}
	}
	return nil
}

func waitForInitContainerToStart(kubeClient kubernetes.Interface, ns string, pod *corev1.Pod) error {
	if kube.HasInitContainerStarted(pod) {
		return nil
	}
	log.Infof("waiting for pod %s to start...\n", pod.Name)
	for {
		time.Sleep(time.Second)

		p, err := kubeClient.CoreV1().Pods(ns).Get(pod.Name, metav1.GetOptions{})
		if err != nil {
			return errors.Wrapf(err, "failed to load pod %s", pod.Name)
		}
		if kube.HasInitContainerStarted(p) {
			return nil
		}
	}
}

func (o *GetBuildLogsOptions) getPodLog(ns string, pod *corev1.Pod, container corev1.Container) error {
	log.Infof("Getting the pod log for pod %s and init container %s\n", pod.Name, container.Name)
	return o.tailLogs(ns, pod.Name, container.Name)
}
