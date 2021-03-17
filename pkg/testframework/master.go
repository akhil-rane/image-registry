package testframework

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/docker/distribution"
	"github.com/pborman/uuid"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	authorizationapiv1 "github.com/openshift/api/authorization/v1"
	projectapiv1 "github.com/openshift/api/project/v1"
	authorizationv1 "github.com/openshift/client-go/authorization/clientset/versioned/typed/authorization/v1"
)

type MasterInterface interface {
	Stop() error
	WaitHealthz(configDir string) error
	AdminKubeConfigPath() string
}

type MasterProcess struct {
	kubeconfig string
}

func StartMasterProcess(kubeconfig string) (MasterInterface, error) {
	if err := os.Setenv("KUBECONFIG", kubeconfig); err != nil {
		return nil, err
	}
	return &MasterProcess{
		kubeconfig: kubeconfig,
	}, nil
}

func (p *MasterProcess) AdminKubeConfigPath() string {
	return p.kubeconfig
}

func (p *MasterProcess) Stop() error { return nil }

func (p *MasterProcess) WaitHealthz(configDir string) error {
	config, err := ConfigFromFile(p.kubeconfig)
	if err != nil {
		return err
	}
	u, _, err := rest.DefaultServerURL(config.Host, config.APIPath, schema.GroupVersion{}, true)
	if err != nil {
		return err
	}
	// #nosec
	// This is art of the test framework; so no need to verify.
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
	}
	rt := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
			DualStack: true,
		}).DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       tlsConfig,
	}

	return WaitHTTP(rt, fmt.Sprintf("https://%s/healthz", u.Host))
}

type User struct {
	Name       string
	Token      string
	kubeConfig *rest.Config
}

func (u *User) KubeConfig() *rest.Config {
	return u.kubeConfig
}

type Repository struct {
	distribution.Repository
	baseURL   string
	repoName  string
	transport http.RoundTripper
}

func (r *Repository) BaseURL() string {
	return r.baseURL
}

func (r *Repository) RepoName() string {
	return r.repoName
}

func (r *Repository) Transport() http.RoundTripper {
	return r.transport
}

type Master struct {
	t               *testing.T
	container       MasterInterface
	adminKubeConfig *rest.Config
	namespaces      []string
}

func NewMaster(t *testing.T) *Master {
	var container MasterInterface
	var err error
	if path, ok := os.LookupEnv("TEST_KUBECONFIG"); ok {
		container, err = StartMasterProcess(path)
	} else if path, ok := os.LookupEnv("KUBECONFIG"); ok {
		container, err = StartMasterProcess(path)
	} else {
		t.Fatalf("tests should be run with either TEST_KUBECONFIG or KUBECONFIG")
	}
	if err != nil {
		t.Fatal(err)
	}

	m := &Master{
		t:         t,
		container: container,
	}
	if err := m.WaitForRoles(); err != nil {
		t.Fatal(err)
	}
	return m
}

func (m *Master) WaitForRoles() error {
	// wait until the cluster roles have been aggregated
	err := wait.Poll(time.Second, time.Minute, func() (bool, error) {
		kubeClient, err := kubeclient.NewForConfig(m.AdminKubeConfig())
		if err != nil {
			return false, err
		}
		for _, roleName := range []string{"admin", "edit", "view"} {
			role, err := kubeClient.RbacV1().ClusterRoles().Get(context.Background(), roleName, metav1.GetOptions{})
			if kerrors.IsNotFound(err) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			if len(role.Rules) == 0 {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		m.t.Fatalf("cluster roles did not aggregate: %v", err)
	}
	return err
}

func (m *Master) Close() {
	if kubeClient, err := kubeclient.NewForConfig(m.AdminKubeConfig()); err == nil {
		for _, ns := range m.namespaces {
			if err := kubeClient.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{}); err != nil {
				m.t.Logf("failed to cleanup namespace %s: %v", ns, err)
			}
		}
	}

	if err := m.container.Stop(); err != nil {
		m.t.Logf("failed to stop the master container: %v", err)
	}
}

func (m *Master) AdminKubeConfig() *rest.Config {
	if m.adminKubeConfig != nil {
		return m.adminKubeConfig
	}

	config, err := ConfigFromFile(m.container.AdminKubeConfigPath())
	if err != nil {
		m.t.Fatalf("failed to read the admin kubeconfig file: %v", err)
	}

	m.adminKubeConfig = config

	return config
}

func (m *Master) StartRegistry(t *testing.T, options ...RegistryOption) *Registry {
	ln, closeFn := StartTestRegistry(t, m.container.AdminKubeConfigPath(), options...)
	return &Registry{
		t:        t,
		listener: ln,
		closeFn:  closeFn,
	}
}

func (m *Master) CreateUser(username string, password string) *User {
	_, user, err := GetClientForUser(m.AdminKubeConfig(), username)
	if err != nil {
		m.t.Fatalf("failed to get a token for the user %s: %v", username, err)
	}
	return &User{
		Name:       username,
		Token:      user.BearerToken,
		kubeConfig: UserClientConfig(m.AdminKubeConfig(), user.BearerToken),
	}
}

func (m *Master) GrantPrunerRole(user *User) {
	authorizationClient := authorizationv1.NewForConfigOrDie(m.AdminKubeConfig())
	_, err := authorizationClient.ClusterRoleBindings().Create(context.Background(), &authorizationapiv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "image-registry-test-pruner-" + uuid.NewRandom().String(),
		},
		UserNames: []string{user.Name},
		RoleRef: corev1.ObjectReference{
			Name: "system:image-pruner",
		},
	}, metav1.CreateOptions{})
	if err != nil {
		m.t.Fatalf("failed to grant the system:image-pruner role to the user %s: %v", user.Name, err)
	}
}

func (m *Master) CreateProject(namespace, user string) *projectapiv1.Project {
	m.namespaces = append(m.namespaces, namespace)
	return CreateProject(m.t, m.AdminKubeConfig(), namespace, user)
}
