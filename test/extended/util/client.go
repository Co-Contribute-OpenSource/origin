package util

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	clientcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"

	yaml "gopkg.in/yaml.v2"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	g "github.com/onsi/ginkgo"
	o "github.com/onsi/gomega"
	"github.com/pborman/uuid"

	kubeauthorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/storage/names"
	memory "k8s.io/client-go/discovery/cached"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	watchtools "k8s.io/client-go/tools/watch"
	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/kubernetes/test/e2e/framework"

	oauthv1 "github.com/openshift/api/oauth/v1"
	projectv1 "github.com/openshift/api/project/v1"
	userv1 "github.com/openshift/api/user/v1"
	appsv1client "github.com/openshift/client-go/apps/clientset/versioned"
	authorizationv1client "github.com/openshift/client-go/authorization/clientset/versioned"
	buildv1client "github.com/openshift/client-go/build/clientset/versioned"
	configv1client "github.com/openshift/client-go/config/clientset/versioned"
	imagev1client "github.com/openshift/client-go/image/clientset/versioned"
	oauthv1client "github.com/openshift/client-go/oauth/clientset/versioned"
	operatorv1client "github.com/openshift/client-go/operator/clientset/versioned"
	projectv1client "github.com/openshift/client-go/project/clientset/versioned"
	quotav1client "github.com/openshift/client-go/quota/clientset/versioned"
	routev1client "github.com/openshift/client-go/route/clientset/versioned"
	securityv1client "github.com/openshift/client-go/security/clientset/versioned"
	templatev1client "github.com/openshift/client-go/template/clientset/versioned"
	userv1client "github.com/openshift/client-go/user/clientset/versioned"
)

// CLI provides function to call the OpenShift CLI and Kubernetes and OpenShift
// clients.
type CLI struct {
	execPath           string
	verb               string
	configPath         string
	adminConfigPath    string
	token              string
	username           string
	globalArgs         []string
	commandArgs        []string
	finalArgs          []string
	namespacesToDelete []string
	stdin              *bytes.Buffer
	stdout             io.Writer
	stderr             io.Writer
	verbose            bool
	withoutNamespace   bool
	kubeFramework      *framework.Framework

	resourcesToDelete []resourceRef
}

type resourceRef struct {
	Resource  schema.GroupVersionResource
	Namespace string
	Name      string
}

// NewCLIWithFramework initializes the CLI using the provided Kube
// framework. It can be called inside of a Ginkgo .It() function.
func NewCLIWithFramework(kubeFramework *framework.Framework) *CLI {
	cli := &CLI{
		kubeFramework:   kubeFramework,
		username:        "admin",
		execPath:        "oc",
		adminConfigPath: KubeConfigPath(),
	}
	return cli
}

// NewCLI initializes the CLI and Kube framework helpers with the provided
// namespace. Should be called outside of a Ginkgo .It() function.
func NewCLI(project string) *CLI {
	cli := NewCLIWithoutNamespace(project)
	cli.withoutNamespace = false
	// create our own project
	g.BeforeEach(func() { _ = cli.SetupProject() })
	return cli
}

// NewCLIWithoutNamespace initializes the CLI and Kube framework helpers
// without a namespace. Should be called outside of a Ginkgo .It()
// function. Use SetupProject() to create a project for this namespace.
func NewCLIWithoutNamespace(project string) *CLI {
	cli := &CLI{
		kubeFramework: &framework.Framework{
			SkipNamespaceCreation:    true,
			BaseName:                 project,
			AddonResourceConstraints: make(map[string]framework.ResourceConstraint),
			Options: framework.Options{
				ClientQPS:   20,
				ClientBurst: 50,
			},
			Timeouts: framework.NewTimeoutContextWithDefaults(),
		},
		username:         "admin",
		execPath:         "oc",
		adminConfigPath:  KubeConfigPath(),
		withoutNamespace: true,
	}
	g.AfterEach(cli.TeardownProject)
	g.AfterEach(cli.kubeFramework.AfterEach)
	g.BeforeEach(cli.kubeFramework.BeforeEach)
	return cli
}

// KubeFramework returns Kubernetes framework which contains helper functions
// specific for Kubernetes resources
func (c *CLI) KubeFramework() *framework.Framework {
	return c.kubeFramework
}

// Username returns the name of currently logged user. If there is no user assigned
// for the current session, it returns 'admin'.
func (c *CLI) Username() string {
	return c.username
}

// AsAdmin changes current config file path to the admin config.
func (c *CLI) AsAdmin() *CLI {
	nc := *c
	nc.configPath = c.adminConfigPath
	return &nc
}

// ChangeUser changes the user used by the current CLI session.
func (c *CLI) ChangeUser(name string) *CLI {
	requiresTestStart()
	clientConfig := c.GetClientConfigForUser(name)

	kubeConfig, err := createConfig(c.Namespace(), clientConfig)
	if err != nil {
		FatalErr(err)
	}

	f, err := ioutil.TempFile("", "configfile")
	if err != nil {
		FatalErr(err)
	}
	c.configPath = f.Name()
	err = clientcmd.WriteToFile(*kubeConfig, c.configPath)
	if err != nil {
		FatalErr(err)
	}

	c.username = name
	framework.Logf("configPath is now %q", c.configPath)
	return c
}

// SetNamespace sets a new namespace
func (c *CLI) SetNamespace(ns string) *CLI {
	c.kubeFramework.Namespace = &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: ns,
		},
	}
	return c
}

// WithoutNamespace instructs the command should be invoked without adding --namespace parameter
func (c CLI) WithoutNamespace() *CLI {
	c.withoutNamespace = true
	return &c
}

// WithToken instructs the command should be invoked with --token rather than --kubeconfig flag
func (c CLI) WithToken(token string) *CLI {
	c.configPath = ""
	c.token = token
	return &c
}

// SetupNamespace creates a namespace, without waiting for any resources except the SCC annotation to be available
func (c *CLI) SetupNamespace() string {
	requiresTestStart()
	newNamespace := names.SimpleNameGenerator.GenerateName(fmt.Sprintf("e2e-test-%s-", c.kubeFramework.BaseName))
	c.SetNamespace(newNamespace)

	framework.Logf("Creating namespace %q", newNamespace)
	_, err := c.AdminKubeClient().CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: newNamespace},
	}, metav1.CreateOptions{})
	o.Expect(err).NotTo(o.HaveOccurred())

	c.kubeFramework.AddNamespacesToDelete(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: newNamespace}})

	WaitForNamespaceSCCAnnotations(c.AdminKubeClient().CoreV1(), newNamespace)
	for _, sa := range []string{"default"} {
		framework.Logf("Waiting for ServiceAccount %q to be provisioned...", sa)
		err = WaitForServiceAccount(c.AdminKubeClient().CoreV1().ServiceAccounts(newNamespace), sa)
		o.Expect(err).NotTo(o.HaveOccurred())
	}
	return newNamespace
}

// SetupProject creates a new project and assign a random user to the project.
// All resources will be then created within this project.
// Returns the name of the new project.
func (c *CLI) SetupProject() string {
	requiresTestStart()
	newNamespace := names.SimpleNameGenerator.GenerateName(fmt.Sprintf("e2e-test-%s-", c.kubeFramework.BaseName))
	c.SetNamespace(newNamespace).ChangeUser(fmt.Sprintf("%s-user", newNamespace))
	framework.Logf("The user is now %q", c.Username())

	framework.Logf("Creating project %q", newNamespace)
	_, err := c.ProjectClient().ProjectV1().ProjectRequests().Create(context.Background(), &projectv1.ProjectRequest{
		ObjectMeta: metav1.ObjectMeta{Name: newNamespace},
	}, metav1.CreateOptions{})
	if apierrors.IsForbidden(err) {
		err = c.setupSelfProvisionerRoleBinding()
	}
	o.Expect(err).NotTo(o.HaveOccurred())

	c.kubeFramework.AddNamespacesToDelete(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: newNamespace}})

	framework.Logf("Waiting on permissions in project %q ...", newNamespace)
	err = WaitForSelfSAR(1*time.Second, 60*time.Second, c.KubeClient(), kubeauthorizationv1.SelfSubjectAccessReviewSpec{
		ResourceAttributes: &kubeauthorizationv1.ResourceAttributes{
			Namespace: newNamespace,
			Verb:      "create",
			Group:     "",
			Resource:  "pods",
		},
	})
	o.Expect(err).NotTo(o.HaveOccurred())

	// Wait for SAs and default dockercfg Secret to be injected
	// TODO: it would be nice to have a shared list but it is defined in at least 3 place,
	// TODO: some of them not even using the constants
	DefaultServiceAccounts := []string{
		"default",
		"deployer",
		"builder",
	}
	for _, sa := range DefaultServiceAccounts {
		framework.Logf("Waiting for ServiceAccount %q to be provisioned...", sa)
		err = WaitForServiceAccount(c.KubeClient().CoreV1().ServiceAccounts(newNamespace), sa)
		o.Expect(err).NotTo(o.HaveOccurred())
	}

	var ctx context.Context
	cancel := func() {}
	defer func() { cancel() }()
	// Wait for default role bindings for those SAs
	for _, name := range []string{"system:image-pullers", "system:image-builders", "system:deployers"} {
		framework.Logf("Waiting for RoleBinding %q to be provisioned...", name)

		ctx, cancel = watchtools.ContextWithOptionalTimeout(context.Background(), 3*time.Minute)

		fieldSelector := fields.OneTermEqualSelector("metadata.name", name).String()
		lw := &cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				options.FieldSelector = fieldSelector
				return c.KubeClient().RbacV1().RoleBindings(newNamespace).List(context.Background(), options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				options.FieldSelector = fieldSelector
				return c.KubeClient().RbacV1().RoleBindings(newNamespace).Watch(context.Background(), options)
			},
		}

		_, err := watchtools.UntilWithSync(ctx, lw, &rbacv1.RoleBinding{}, nil, func(event watch.Event) (b bool, e error) {
			switch t := event.Type; t {
			case watch.Added, watch.Modified:
				return true, nil

			case watch.Deleted:
				return true, fmt.Errorf("object has been deleted")

			default:
				return true, fmt.Errorf("internal error: unexpected event %#v", e)
			}
		})
		o.Expect(err).NotTo(o.HaveOccurred())
	}

	WaitForProjectSCCAnnotations(c.ProjectClient().ProjectV1(), newNamespace)

	framework.Logf("Project %q has been fully provisioned.", newNamespace)
	return newNamespace
}

func (c *CLI) setupSelfProvisionerRoleBinding() error {
	framework.Logf("Creating role binding to allow self provisioning of projects")
	rb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "e2e-self-provisioners",
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.SchemeGroupVersion.Group,
			Kind:     "ClusterRole",
			Name:     "self-provisioner",
		},
		Subjects: []rbacv1.Subject{
			{
				APIGroup: rbacv1.SchemeGroupVersion.Group,
				Kind:     "Group",
				Name:     "system:authenticated:oauth",
			},
		},
	}
	_, err := c.AdminKubeClient().RbacV1().ClusterRoleBindings().Create(context.Background(), rb, metav1.CreateOptions{})
	return err
}

// SetupProject creates a new project and assign a random user to the project.
// All resources will be then created within this project.
// TODO this should be removed.  It's only used by image tests.
func (c *CLI) CreateProject() string {
	requiresTestStart()
	newNamespace := names.SimpleNameGenerator.GenerateName(fmt.Sprintf("e2e-test-%s-", c.kubeFramework.BaseName))
	framework.Logf("Creating project %q", newNamespace)
	_, err := c.ProjectClient().ProjectV1().ProjectRequests().Create(context.Background(), &projectv1.ProjectRequest{
		ObjectMeta: metav1.ObjectMeta{Name: newNamespace},
	}, metav1.CreateOptions{})
	if apierrors.IsForbidden(err) {
		err = c.setupSelfProvisionerRoleBinding()
	}
	o.Expect(err).NotTo(o.HaveOccurred())

	actualNs, err := c.AdminKubeClient().CoreV1().Namespaces().Get(context.Background(), newNamespace, metav1.GetOptions{})
	o.Expect(err).NotTo(o.HaveOccurred())
	c.kubeFramework.AddNamespacesToDelete(actualNs)

	framework.Logf("Waiting on permissions in project %q ...", newNamespace)
	err = WaitForSelfSAR(1*time.Second, 60*time.Second, c.KubeClient(), kubeauthorizationv1.SelfSubjectAccessReviewSpec{
		ResourceAttributes: &kubeauthorizationv1.ResourceAttributes{
			Namespace: newNamespace,
			Verb:      "create",
			Group:     "",
			Resource:  "pods",
		},
	})
	o.Expect(err).NotTo(o.HaveOccurred())
	return newNamespace
}

// TeardownProject removes projects created by this test.
func (c *CLI) TeardownProject() {
	if len(c.Namespace()) > 0 && g.CurrentGinkgoTestDescription().Failed && framework.TestContext.DumpLogsOnFailure {
		framework.DumpAllNamespaceInfo(c.kubeFramework.ClientSet, c.Namespace())
	}

	if len(c.configPath) > 0 {
		os.Remove(c.configPath)
	}

	dynamicClient := c.AdminDynamicClient()
	for _, resource := range c.resourcesToDelete {
		err := dynamicClient.Resource(resource.Resource).Namespace(resource.Namespace).Delete(context.Background(), resource.Name, metav1.DeleteOptions{})
		framework.Logf("Deleted %v, err: %v", resource, err)
	}
}

// Verbose turns on printing verbose messages when executing OpenShift commands
func (c *CLI) Verbose() *CLI {
	c.verbose = true
	return c
}

func (c *CLI) RESTMapper() meta.RESTMapper {
	ret := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(c.KubeClient().Discovery()))
	ret.Reset()
	return ret
}

func (c *CLI) AppsClient() appsv1client.Interface {
	return appsv1client.NewForConfigOrDie(c.UserConfig())
}

func (c *CLI) AuthorizationClient() authorizationv1client.Interface {
	return authorizationv1client.NewForConfigOrDie(c.UserConfig())
}

func (c *CLI) BuildClient() buildv1client.Interface {
	return buildv1client.NewForConfigOrDie(c.UserConfig())
}

func (c *CLI) ImageClient() imagev1client.Interface {
	return imagev1client.NewForConfigOrDie(c.UserConfig())
}

func (c *CLI) ProjectClient() projectv1client.Interface {
	return projectv1client.NewForConfigOrDie(c.UserConfig())
}

func (c *CLI) QuotaClient() quotav1client.Interface {
	return quotav1client.NewForConfigOrDie(c.UserConfig())
}

func (c *CLI) RouteClient() routev1client.Interface {
	return routev1client.NewForConfigOrDie(c.UserConfig())
}

func (c *CLI) TemplateClient() templatev1client.Interface {
	return templatev1client.NewForConfigOrDie(c.UserConfig())
}

func (c *CLI) AdminAppsClient() appsv1client.Interface {
	return appsv1client.NewForConfigOrDie(c.AdminConfig())
}

func (c *CLI) AdminAuthorizationClient() authorizationv1client.Interface {
	return authorizationv1client.NewForConfigOrDie(c.AdminConfig())
}

func (c *CLI) AdminBuildClient() buildv1client.Interface {
	return buildv1client.NewForConfigOrDie(c.AdminConfig())
}

func (c *CLI) AdminConfigClient() configv1client.Interface {
	return configv1client.NewForConfigOrDie(c.AdminConfig())
}

func (c *CLI) AdminImageClient() imagev1client.Interface {
	return imagev1client.NewForConfigOrDie(c.AdminConfig())
}

func (c *CLI) AdminOauthClient() oauthv1client.Interface {
	return oauthv1client.NewForConfigOrDie(c.AdminConfig())
}

func (c *CLI) AdminOperatorClient() operatorv1client.Interface {
	return operatorv1client.NewForConfigOrDie(c.AdminConfig())
}

func (c *CLI) AdminProjectClient() projectv1client.Interface {
	return projectv1client.NewForConfigOrDie(c.AdminConfig())
}

func (c *CLI) AdminQuotaClient() quotav1client.Interface {
	return quotav1client.NewForConfigOrDie(c.AdminConfig())
}

func (c *CLI) AdminOAuthClient() oauthv1client.Interface {
	return oauthv1client.NewForConfigOrDie(c.AdminConfig())
}

func (c *CLI) AdminRouteClient() routev1client.Interface {
	return routev1client.NewForConfigOrDie(c.AdminConfig())
}

func (c *CLI) AdminUserClient() userv1client.Interface {
	return userv1client.NewForConfigOrDie(c.AdminConfig())
}

func (c *CLI) AdminSecurityClient() securityv1client.Interface {
	return securityv1client.NewForConfigOrDie(c.AdminConfig())
}

func (c *CLI) AdminTemplateClient() templatev1client.Interface {
	return templatev1client.NewForConfigOrDie(c.AdminConfig())
}

// KubeClient provides a Kubernetes client for the current namespace
func (c *CLI) KubeClient() kubernetes.Interface {
	return kubernetes.NewForConfigOrDie(c.UserConfig())
}

func (c *CLI) DynamicClient() dynamic.Interface {
	return dynamic.NewForConfigOrDie(c.UserConfig())
}

// AdminKubeClient provides a Kubernetes client for the cluster admin user.
func (c *CLI) AdminKubeClient() kubernetes.Interface {
	return kubernetes.NewForConfigOrDie(c.AdminConfig())
}

func (c *CLI) AdminDynamicClient() dynamic.Interface {
	return dynamic.NewForConfigOrDie(c.AdminConfig())
}

func (c *CLI) UserConfig() *rest.Config {
	clientConfig, err := getClientConfig(c.configPath)
	if err != nil {
		FatalErr(err)
	}
	return clientConfig
}

func (c *CLI) AdminConfig() *rest.Config {
	clientConfig, err := getClientConfig(c.adminConfigPath)
	if err != nil {
		FatalErr(err)
	}
	return clientConfig
}

// Namespace returns the name of the namespace used in the current test case.
// If the namespace is not set, an empty string is returned.
func (c *CLI) Namespace() string {
	if c.kubeFramework.Namespace == nil {
		return ""
	}
	return c.kubeFramework.Namespace.Name
}

// setOutput allows to override the default command output
func (c *CLI) setOutput(out io.Writer) *CLI {
	c.stdout = out
	return c
}

// Run executes given OpenShift CLI command verb (iow. "oc <verb>").
// This function also override the default 'stdout' to redirect all output
// to a buffer and prepare the global flags such as namespace and config path.
func (c *CLI) Run(commands ...string) *CLI {
	requiresTestStart()
	in, out, errout := &bytes.Buffer{}, &bytes.Buffer{}, &bytes.Buffer{}
	nc := &CLI{
		execPath:        c.execPath,
		verb:            commands[0],
		kubeFramework:   c.KubeFramework(),
		adminConfigPath: c.adminConfigPath,
		configPath:      c.configPath,
		username:        c.username,
		globalArgs:      commands,
	}
	if len(c.configPath) > 0 {
		nc.globalArgs = append([]string{fmt.Sprintf("--kubeconfig=%s", c.configPath)}, nc.globalArgs...)
	}
	if len(c.configPath) == 0 && len(c.token) > 0 {
		nc.globalArgs = append([]string{fmt.Sprintf("--token=%s", c.token)}, nc.globalArgs...)
	}
	if !c.withoutNamespace {
		nc.globalArgs = append([]string{fmt.Sprintf("--namespace=%s", c.Namespace())}, nc.globalArgs...)
	}
	nc.stdin, nc.stdout, nc.stderr = in, out, errout
	return nc.setOutput(c.stdout)
}

// InputString adds expected input to the command
func (c *CLI) InputString(input string) *CLI {
	c.stdin.WriteString(input)
	return c
}

// Args sets the additional arguments for the OpenShift CLI command
func (c *CLI) Args(args ...string) *CLI {
	c.commandArgs = args
	return c
}

func (c *CLI) printCmd() string {
	return strings.Join(c.finalArgs, " ")
}

type ExitError struct {
	Cmd    string
	StdErr string
	*exec.ExitError
}

// Output executes the command and returns stdout/stderr combined into one string
func (c *CLI) Output() (string, error) {
	var buff bytes.Buffer
	_, _, err := c.outputs(&buff, &buff)
	return strings.TrimSpace(string(buff.Bytes())), err
}

// Outputs executes the command and returns the stdout/stderr output as separate strings
func (c *CLI) Outputs() (string, string, error) {
	var stdOutBuff, stdErrBuff bytes.Buffer
	return c.outputs(&stdOutBuff, &stdErrBuff)
}

// Background executes the command in the background and returns the Cmd object
// which may be killed later via cmd.Process.Kill().  It also returns buffers
// holding the stdout & stderr of the command, which may be read from only after
// calling cmd.Wait().
func (c *CLI) Background() (*exec.Cmd, *bytes.Buffer, *bytes.Buffer, error) {
	var stdOutBuff, stdErrBuff bytes.Buffer
	cmd, err := c.start(&stdOutBuff, &stdErrBuff)
	return cmd, &stdOutBuff, &stdErrBuff, err
}

func (c *CLI) start(stdOutBuff, stdErrBuff *bytes.Buffer) (*exec.Cmd, error) {
	c.finalArgs = append(c.globalArgs, c.commandArgs...)
	if c.verbose {
		fmt.Printf("DEBUG: oc %s\n", c.printCmd())
	}
	cmd := exec.Command(c.execPath, c.finalArgs...)
	cmd.Stdin = c.stdin
	framework.Logf("Running '%s %s'", c.execPath, strings.Join(c.finalArgs, " "))

	cmd.Stdout = stdOutBuff
	cmd.Stderr = stdErrBuff
	err := cmd.Start()

	return cmd, err
}

func (c *CLI) outputs(stdOutBuff, stdErrBuff *bytes.Buffer) (string, string, error) {
	cmd, err := c.start(stdOutBuff, stdErrBuff)
	if err != nil {
		return "", "", err
	}
	err = cmd.Wait()

	stdOutBytes := stdOutBuff.Bytes()
	stdErrBytes := stdErrBuff.Bytes()
	stdOut := strings.TrimSpace(string(stdOutBytes))
	stdErr := strings.TrimSpace(string(stdErrBytes))

	switch err.(type) {
	case nil:
		c.stdout = bytes.NewBuffer(stdOutBytes)
		c.stderr = bytes.NewBuffer(stdErrBytes)
		return stdOut, stdErr, nil
	case *exec.ExitError:
		framework.Logf("Error running %v:\nStdOut>\n%s\nStdErr>\n%s\n", cmd, stdOut, stdErr)
		return stdOut, stdErr, err
	default:
		FatalErr(fmt.Errorf("unable to execute %q: %v", c.execPath, err))
		// unreachable code
		return "", "", nil
	}
}

// OutputToFile executes the command and store output to a file
func (c *CLI) OutputToFile(filename string) (string, error) {
	content, _, err := c.Outputs()
	if err != nil {
		return "", err
	}
	path := filepath.Join(framework.TestContext.OutputDir, c.Namespace()+"-"+filename)
	return path, ioutil.WriteFile(path, []byte(content), 0644)
}

// Execute executes the current command and return error if the execution failed
// This function will set the default output to Ginkgo writer.
func (c *CLI) Execute() error {
	out, err := c.Output()
	if _, err := io.Copy(g.GinkgoWriter, strings.NewReader(out+"\n")); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR: Unable to copy the output to ginkgo writer")
	}
	os.Stdout.Sync()
	return err
}

// FatalErr exits the test in case a fatal error has occurred.
func FatalErr(msg interface{}) {
	// the path that leads to this being called isn't always clear...
	fmt.Fprintln(g.GinkgoWriter, string(debug.Stack()))
	framework.Failf("%v", msg)
}

func (c *CLI) AddExplicitResourceToDelete(resource schema.GroupVersionResource, namespace, name string) {
	c.resourcesToDelete = append(c.resourcesToDelete, resourceRef{Resource: resource, Namespace: namespace, Name: name})
}

func (c *CLI) AddResourceToDelete(resource schema.GroupVersionResource, metadata metav1.Object) {
	c.resourcesToDelete = append(c.resourcesToDelete, resourceRef{Resource: resource, Namespace: metadata.GetNamespace(), Name: metadata.GetName()})
}

func (c *CLI) CreateUser(prefix string) *userv1.User {
	user, err := c.AdminUserClient().UserV1().Users().Create(context.Background(), &userv1.User{
		ObjectMeta: metav1.ObjectMeta{GenerateName: prefix + c.Namespace()},
	}, metav1.CreateOptions{})
	if err != nil {
		FatalErr(err)
	}
	c.AddResourceToDelete(userv1.GroupVersion.WithResource("users"), user)

	return user
}

func (c *CLI) GetClientConfigForUser(username string) *rest.Config {
	ctx := context.Background()
	userClient := c.AdminUserClient()

	user, err := userClient.UserV1().Users().Get(ctx, username, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		FatalErr(err)
	}
	if err != nil {
		user, err = userClient.UserV1().Users().Create(ctx, &userv1.User{
			ObjectMeta: metav1.ObjectMeta{Name: username},
		}, metav1.CreateOptions{})
		if err != nil {
			FatalErr(err)
		}
		c.AddResourceToDelete(userv1.GroupVersion.WithResource("users"), user)
	}

	oauthClient := c.AdminOauthClient()
	oauthClientName := "e2e-client-" + c.Namespace()
	oauthClientObj, err := oauthClient.OauthV1().OAuthClients().Create(ctx, &oauthv1.OAuthClient{
		ObjectMeta:  metav1.ObjectMeta{Name: oauthClientName},
		GrantMethod: oauthv1.GrantHandlerAuto,
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		FatalErr(err)
	}
	if oauthClientObj != nil {
		c.AddExplicitResourceToDelete(oauthv1.GroupVersion.WithResource("oauthclients"), "", oauthClientName)
	}

	privToken, pubToken := GenerateOAuthTokenPair()
	token, err := oauthClient.OauthV1().OAuthAccessTokens().Create(ctx, &oauthv1.OAuthAccessToken{
		ObjectMeta:  metav1.ObjectMeta{Name: pubToken},
		ClientName:  oauthClientName,
		UserName:    username,
		UserUID:     string(user.UID),
		Scopes:      []string{"user:full"},
		RedirectURI: "https://localhost:8443/oauth/token/implicit",
	}, metav1.CreateOptions{})
	if err != nil {
		FatalErr(err)
	}
	c.AddResourceToDelete(oauthv1.GroupVersion.WithResource("oauthaccesstokens"), token)

	userClientConfig := rest.AnonymousClientConfig(turnOffRateLimiting(rest.CopyConfig(c.AdminConfig())))
	userClientConfig.BearerToken = privToken

	return userClientConfig
}

// GenerateOAuthTokenPair returns two tokens to use with OpenShift OAuth-based authentication.
// The first token is a private token meant to be used as a Bearer token to send
// queries to the API, the second token is a hashed token meant to be stored in
// the database.
func GenerateOAuthTokenPair() (privToken, pubToken string) {
	const sha256Prefix = "sha256~"
	randomToken := base64.RawURLEncoding.EncodeToString(uuid.NewRandom())
	hashed := sha256.Sum256([]byte(randomToken))
	return sha256Prefix + string(randomToken), sha256Prefix + base64.RawURLEncoding.EncodeToString(hashed[:])
}

// turnOffRateLimiting reduces the chance that a flaky test can be written while using this package
func turnOffRateLimiting(config *rest.Config) *rest.Config {
	configCopy := *config
	configCopy.QPS = 10000
	configCopy.Burst = 10000
	configCopy.RateLimiter = flowcontrol.NewFakeAlwaysRateLimiter()
	// We do not set a timeout because that will cause watches to fail
	// Integration tests are already limited to 5 minutes
	// configCopy.Timeout = time.Minute
	return &configCopy
}

func (c *CLI) WaitForAccessAllowed(review *kubeauthorizationv1.SelfSubjectAccessReview, user string) error {
	if user == "system:anonymous" {
		return waitForAccess(kubernetes.NewForConfigOrDie(rest.AnonymousClientConfig(c.AdminConfig())), true, review)
	}

	kubeClient, err := kubernetes.NewForConfig(c.GetClientConfigForUser(user))
	if err != nil {
		FatalErr(err)
	}
	return waitForAccess(kubeClient, true, review)
}

func (c *CLI) WaitForAccessDenied(review *kubeauthorizationv1.SelfSubjectAccessReview, user string) error {
	if user == "system:anonymous" {
		return waitForAccess(kubernetes.NewForConfigOrDie(rest.AnonymousClientConfig(c.AdminConfig())), false, review)
	}

	kubeClient, err := kubernetes.NewForConfig(c.GetClientConfigForUser(user))
	if err != nil {
		FatalErr(err)
	}
	return waitForAccess(kubeClient, false, review)
}

func waitForAccess(c kubernetes.Interface, allowed bool, review *kubeauthorizationv1.SelfSubjectAccessReview) error {
	return wait.Poll(time.Second, time.Minute, func() (bool, error) {
		response, err := c.AuthorizationV1().SelfSubjectAccessReviews().Create(context.Background(), review, metav1.CreateOptions{})
		if err != nil {
			return false, err
		}
		return response.Status.Allowed == allowed, nil
	})
}

func getClientConfig(kubeConfigFile string) (*rest.Config, error) {
	kubeConfigBytes, err := ioutil.ReadFile(kubeConfigFile)
	if err != nil {
		return nil, err
	}
	kubeConfig, err := clientcmd.NewClientConfigFromBytes(kubeConfigBytes)
	if err != nil {
		return nil, err
	}
	clientConfig, err := kubeConfig.ClientConfig()
	if err != nil {
		return nil, err
	}
	clientConfig.WrapTransport = defaultClientTransport

	return clientConfig, nil
}

// defaultClientTransport sets defaults for a client Transport that are suitable
// for use by infrastructure components.
func defaultClientTransport(rt http.RoundTripper) http.RoundTripper {
	transport, ok := rt.(*http.Transport)
	if !ok {
		return rt
	}

	// TODO: this should be configured by the caller, not in this method.
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	transport.Dial = dialer.Dial
	// Hold open more internal idle connections
	// TODO: this should be configured by the caller, not in this method.
	transport.MaxIdleConnsPerHost = 100
	return transport
}

const (
	installConfigName = "cluster-config-v1"
)

// installConfig The subset of openshift-install's InstallConfig we parse for this test
type installConfig struct {
	FIPS bool `json:"fips,omitempty"`
}

func IsFIPS(client clientcorev1.ConfigMapsGetter) (bool, error) {
	// this currently uses an install config because it has a lower dependency threshold than going directly to the node.
	installConfig, err := installConfigFromCluster(client)
	if err != nil {
		return false, err
	}
	return installConfig.FIPS, nil
}

func installConfigFromCluster(client clientcorev1.ConfigMapsGetter) (*installConfig, error) {
	cm, err := client.ConfigMaps("kube-system").Get(context.Background(), installConfigName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	data, ok := cm.Data["install-config"]
	if !ok {
		return nil, fmt.Errorf("no install-config found in kube-system/%s", installConfigName)
	}
	config := &installConfig{}
	if err := yaml.Unmarshal([]byte(data), config); err != nil {
		return nil, err
	}
	return config, nil
}
