/*
Copyright 2017 The Kubernetes Authors.

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

package server

import (
	"fmt"
	"os"
	"strings"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/linux/runcopts"
	"github.com/containerd/typeurl"
	"github.com/cri-o/ocicni/pkg/ocicni"
	"github.com/golang/glog"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	runtimespec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/runtime-tools/generate"
	"golang.org/x/net/context"
	"golang.org/x/sys/unix"
	"k8s.io/kubernetes/pkg/kubelet/apis/cri/v1alpha1/runtime"

	customopts "github.com/kubernetes-incubator/cri-containerd/pkg/containerd/opts"
	sandboxstore "github.com/kubernetes-incubator/cri-containerd/pkg/store/sandbox"
	"github.com/kubernetes-incubator/cri-containerd/pkg/util"
)

func init() {
	typeurl.Register(&sandboxstore.Metadata{},
		"github.com/kubernetes-incubator/cri-containerd/pkg/store/sandbox", "Metadata")
}

// RunPodSandbox creates and starts a pod-level sandbox. Runtimes should ensure
// the sandbox is in ready state.
// RunPodSandbox创建并启动一个pod-level sandbox，runtime必须确保sandbox处于ready状态
func (c *criContainerdService) RunPodSandbox(ctx context.Context, r *runtime.RunPodSandboxRequest) (_ *runtime.RunPodSandboxResponse, retErr error) {
	config := r.GetConfig()

	// Generate unique id and name for the sandbox and reserve the name.
	// 创建sandbox的id和name
	id := util.GenerateID()
	name := makeSandboxName(config.GetMetadata())
	glog.V(4).Infof("Generated id %q for sandbox %q", id, name)
	// Reserve the sandbox name to avoid concurrent `RunPodSandbox` request starting the
	// same sandbox.
	// 保留sandbox的name和id，防止并发地`RunPodSandbox`请求要求启动容器
	if err := c.sandboxNameIndex.Reserve(name, id); err != nil {
		return nil, fmt.Errorf("failed to reserve sandbox name %q: %v", name, err)
	}
	defer func() {
		// Release the name if the function returns with an error.
		// 如果有错误，则将保留的sandbox name删除
		if retErr != nil {
			c.sandboxNameIndex.ReleaseByName(name)
		}
	}()

	// Create initial internal sandbox object.
	// 创建初始的内部的sandbox对象
	sandbox := sandboxstore.Sandbox{
		Metadata: sandboxstore.Metadata{
			ID:     id,
			Name:   name,
			Config: config,
		},
	}

	// Ensure sandbox container image snapshot.
	// ensureImageExists用来返回镜像的元数据，如果镜像不存在的话，会自动下载镜像
	// 确保镜像”gcr.io/google_containers/pause:3.0"存在
	image, err := c.ensureImageExists(ctx, c.config.SandboxImage)
	if err != nil {
		return nil, fmt.Errorf("failed to get sandbox image %q: %v", c.config.SandboxImage, err)
	}
	securityContext := config.GetLinux().GetSecurityContext()
	//Create Network Namespace if it is not in host network
	hostNet := securityContext.GetNamespaceOptions().GetHostNetwork()
	if !hostNet {
		// If it is not in host network namespace then create a namespace and set the sandbox
		// handle. NetNSPath in sandbox metadata and NetNS is non empty only for non host network
		// namespaces. If the pod is in host network namespace then both are empty and should not
		// be used.
		// 如果sandbox不是在host network namespace中，则创建一个namespace，其中NetNSPath和NetNS都不为空
		// 如果sandbox位于host nentwork namespace中，则NetNSPath和NetNS都为空且不被使用
		sandbox.NetNS, err = sandboxstore.NewNetNS()
		if err != nil {
			return nil, fmt.Errorf("failed to create network namespace for sandbox %q: %v", id, err)
		}
		sandbox.NetNSPath = sandbox.NetNS.GetPath()
		defer func() {
			if retErr != nil {
				if err := sandbox.NetNS.Remove(); err != nil {
					glog.Errorf("Failed to remove network namespace %s for sandbox %q: %v", sandbox.NetNSPath, id, err)
				}
				sandbox.NetNSPath = ""
			}
		}()
		// Setup network for sandbox.
		podNetwork := ocicni.PodNetwork{
			Name:         config.GetMetadata().GetName(),
			// Namespace是sandbox所在的namespace
			Namespace:    config.GetMetadata().GetNamespace(),
			ID:           id,
			// NetNS是sandbox network namespace所在的path
			NetNS:        sandbox.NetNSPath,
			// 将CRI的port mapping转换为CNI的port mapping
			PortMappings: toCNIPortMappings(config.GetPortMappings()),
		}
		if err = c.netPlugin.SetUpPod(podNetwork); err != nil {
			return nil, fmt.Errorf("failed to setup network for sandbox %q: %v", id, err)
		}
		defer func() {
			if retErr != nil {
				// Teardown network if an error is returned.
				if err := c.netPlugin.TearDownPod(podNetwork); err != nil {
					glog.Errorf("Failed to destroy network for sandbox %q: %v", id, err)
				}
			}
		}()
	}

	// Create sandbox container.
	// 创建sandbox container
	spec, err := c.generateSandboxContainerSpec(id, config, image.Config, sandbox.NetNSPath)
	if err != nil {
		return nil, fmt.Errorf("failed to generate sandbox container spec: %v", err)
	}
	glog.V(4).Infof("Sandbox container spec: %+v", spec)

	// specOpts包含用于修改container spec相关的选项
	var specOpts []containerd.SpecOpts
	// user id相关的SpecOpts
	if uid := securityContext.GetRunAsUser(); uid != nil {
		specOpts = append(specOpts, containerd.WithUserID(uint32(uid.GetValue())))
	}

	// 生成seccomp相关的SpecOpts
	seccompSpecOpts, err := generateSeccompSpecOpts(
		securityContext.GetSeccompProfilePath(),
		securityContext.GetPrivileged(),
		c.seccompEnabled)
	if err != nil {
		return nil, fmt.Errorf("failed to generate seccomp spec opts: %v", err)
	}
	if seccompSpecOpts != nil {
		specOpts = append(specOpts, seccompSpecOpts)
	}

	// containerKindSandbox是一个常量"sandbox"，表示container是一个sandbox container
	// buildLabels返回一个map[string]string结构
	sandboxLabels := buildLabels(config.Labels, containerKindSandbox)

	// 设置containrd新建容器的选项
	opts := []containerd.NewContainerOpts{
		// c.config.ContainerConfig.Snapshotter默认为"overlayfs"
		// 将容器配置的snapshotter设置为"overlayfs"
		containerd.WithSnapshotter(c.config.ContainerdConfig.Snapshotter),
		// WithImageUnpack用于确保容器使用的镜像是unpack的
		customopts.WithImageUnpack(image.Image),
		containerd.WithNewSnapshot(id, image.Image),
		containerd.WithSpec(spec, specOpts...),
		containerd.WithContainerLabels(sandboxLabels),
		// 将sandbox的元数据作为extension存储
		containerd.WithContainerExtension(sandboxMetadataExtension, &sandbox.Metadata),
		// runtime相关的选项
		containerd.WithRuntime(
			// Runtime默认为"io.containerd.runtime.v1.linux"
			c.config.ContainerdConfig.Runtime,
			&runcopts.RuncOptions{
				// RuntimeEngine和RuntimeRoot的默认为""
				Runtime:       c.config.ContainerdConfig.RuntimeEngine,
				RuntimeRoot:   c.config.ContainerdConfig.RuntimeRoot,
				SystemdCgroup: c.config.SystemdCgroup})} // TODO (mikebrow): add CriuPath when we add support for pause

	// 调用containerd client创建container
	container, err := c.client.NewContainer(ctx, id, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create containerd container: %v", err)
	}
	defer func() {
		if retErr != nil {
			if err := container.Delete(ctx, containerd.WithSnapshotCleanup); err != nil {
				glog.Errorf("Failed to delete containerd container %q: %v", id, err)
			}
		}
	}()

	// Create sandbox container root directory.
	// c.config.RootDir默认为/var/lib/cri-containerd
	sandboxRootDir := getSandboxRootDir(c.config.RootDir, id)
	// 创建sandbox的根目录/var/lib/cri-containerd/sandboxid/
	if err := c.os.MkdirAll(sandboxRootDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create sandbox root directory %q: %v",
			sandboxRootDir, err)
	}
	defer func() {
		if retErr != nil {
			// Cleanup the sandbox root directory.
			if err := c.os.RemoveAll(sandboxRootDir); err != nil {
				glog.Errorf("Failed to remove sandbox root directory %q: %v",
					sandboxRootDir, err)
			}
		}
	}()

	// Setup sandbox /dev/shm, /etc/hosts and /etc/resolv.conf.
	// 创建sandbox的/dev/shm，/etc/hosts和/etc/resolv.conf文件
	if err = c.setupSandboxFiles(sandboxRootDir, config); err != nil {
		return nil, fmt.Errorf("failed to setup sandbox files: %v", err)
	}
	defer func() {
		if retErr != nil {
			if err = c.unmountSandboxFiles(sandboxRootDir, config); err != nil {
				glog.Errorf("Failed to unmount sandbox files in %q: %v",
					sandboxRootDir, err)
			}
		}
	}()

	// Create sandbox task in containerd.
	// 在containerd中创建sandbox task
	glog.V(5).Infof("Create sandbox container (id=%q, name=%q).",
		id, name)
	// We don't need stdio for sandbox container.
	// 对于sandbox container我们不需要stdio
	// NullIO将容器的stdio导入stdio
	task, err := container.NewTask(ctx, containerd.NullIO)
	if err != nil {
		return nil, fmt.Errorf("failed to create task for sandbox %q: %v", id, err)
	}
	defer func() {
		if retErr != nil {
			// Cleanup the sandbox container if an error is returned.
			if _, err := task.Delete(ctx, containerd.WithProcessKill); err != nil {
				glog.Errorf("Failed to delete sandbox container %q: %v", id, err)
			}
		}
	}()

	// 启动sandbox container task
	if err = task.Start(ctx); err != nil {
		return nil, fmt.Errorf("failed to start sandbox container task %q: %v",
			id, err)
	}

	// Add sandbox into sandbox store.
	// 将sandbox加入sandbox store
	sandbox.Container = container
	if err := c.sandboxStore.Add(sandbox); err != nil {
		return nil, fmt.Errorf("failed to add sandbox %+v into store: %v", sandbox, err)
	}

	return &runtime.RunPodSandboxResponse{PodSandboxId: id}, nil
}

// ImageConfig定义了用镜像启动一个容器使用的执行参数
func (c *criContainerdService) generateSandboxContainerSpec(id string, config *runtime.PodSandboxConfig,
	imageConfig *imagespec.ImageConfig, nsPath string) (*runtimespec.Spec, error) {
	// Creates a spec Generator with the default spec.
	// TODO(random-liu): [P1] Compare the default settings with docker and containerd default.
	// 创建一个cri-containerd默认的spec
	spec, err := defaultRuntimeSpec(id)
	if err != nil {
		return nil, err
	}
	g := generate.NewFromSpec(spec)

	// Apply default config from image config.
	// 应用image config的默认配置，添加环境变量
	if err := addImageEnvs(&g, imageConfig.Env); err != nil {
		return nil, err
	}

	if imageConfig.WorkingDir != "" {
		g.SetProcessCwd(imageConfig.WorkingDir)
	}

	if len(imageConfig.Entrypoint) == 0 {
		// Pause image must have entrypoint.
		// Pause镜像必须有entrypoint
		return nil, fmt.Errorf("invalid empty entrypoint in image config %+v", imageConfig)
	}
	// Set process commands.
	// 添加entrypoint和Cmd
	g.SetProcessArgs(append(imageConfig.Entrypoint, imageConfig.Cmd...))

	// Set relative root path.
	// relativeRootfsPath是值为"rootfs"的常量
	g.SetRootPath(relativeRootfsPath)

	// Make root of sandbox container read-only.
	g.SetRootReadonly(true)

	// Set hostname.
	g.SetHostname(config.GetHostname())

	// TODO(random-liu): [P2] Consider whether to add labels and annotations to the container.

	// Set cgroups parent.
	// 设置cgroups parent
	if config.GetLinux().GetCgroupParent() != "" {
		cgroupsPath := getCgroupsPath(config.GetLinux().GetCgroupParent(), id,
			c.config.SystemdCgroup)
		g.SetLinuxCgroupsPath(cgroupsPath)
	}
	// When cgroup parent is not set, containerd-shim will create container in a child cgroup
	// of the cgroup itself is in.
	// 如果cgroup parent没有设置，那么containerd-shim会在它所在cgroup的子cgroup中创建容器
	// TODO(random-liu): [P2] Set default cgroup path if cgroup parent is not specified.

	// Set namespace options.
	// 设置namespaec选项
	securityContext := config.GetLinux().GetSecurityContext()
	nsOptions := securityContext.GetNamespaceOptions()
	// 如果有namespace设置为host模式，则删除spec中相应的namespace
	if nsOptions.GetHostNetwork() {
		g.RemoveLinuxNamespace(string(runtimespec.NetworkNamespace)) // nolint: errcheck
	} else {
		//TODO(Abhi): May be move this to containerd spec opts (WithLinuxSpaceOption)
		g.AddOrReplaceLinuxNamespace(string(runtimespec.NetworkNamespace), nsPath) // nolint: errcheck
	}
	if nsOptions.GetHostPid() {
		g.RemoveLinuxNamespace(string(runtimespec.PIDNamespace)) // nolint: errcheck
	}

	if nsOptions.GetHostIpc() {
		g.RemoveLinuxNamespace(string(runtimespec.IPCNamespace)) // nolint: errcheck
	}

	selinuxOpt := securityContext.GetSelinuxOptions()
	processLabel, mountLabel, err := initSelinuxOpts(selinuxOpt)
	if err != nil {
		return nil, fmt.Errorf("failed to init selinux options %+v: %v", securityContext.GetSelinuxOptions(), err)
	}
	// 设置selinux的相关选项
	g.SetProcessSelinuxLabel(processLabel)
	g.SetLinuxMountLabel(mountLabel)

	// 设置supplemental group
	supplementalGroups := securityContext.GetSupplementalGroups()
	for _, group := range supplementalGroups {
		g.AddProcessAdditionalGid(uint32(group))
	}

	// Add sysctls
	// 添加sysctl选项
	sysctls := config.GetLinux().GetSysctls()
	for key, value := range sysctls {
		g.AddLinuxSysctl(key, value)
	}

	// Note: LinuxSandboxSecurityContext does not currently provide an apparmor profile

	// 设置sandbox的共享CPU的数目
	g.SetLinuxResourcesCPUShares(uint64(defaultSandboxCPUshares))
	g.SetProcessOOMScoreAdj(int(defaultSandboxOOMAdj))

	// 返回根据镜像配置以及其他一些默认参数修改后的spec
	return g.Spec(), nil
}

// setupSandboxFiles sets up necessary sandbox files including /dev/shm, /etc/hosts
// and /etc/resolv.conf.
func (c *criContainerdService) setupSandboxFiles(rootDir string, config *runtime.PodSandboxConfig) error {
	// TODO(random-liu): Consider whether we should maintain /etc/hosts and /etc/resolv.conf in kubelet.
	sandboxEtcHosts := getSandboxHosts(rootDir)
	// etcHosts是"/etc/hosts"，将/etc/hosts复制到sandboxEtcHosts
	if err := c.os.CopyFile(etcHosts, sandboxEtcHosts, 0644); err != nil {
		return fmt.Errorf("failed to generate sandbox hosts file %q: %v", sandboxEtcHosts, err)
	}

	// Set DNS options. Maintain a resolv.conf for the sandbox.
	var err error
	resolvContent := ""
	// 将config中的dns config转换为resolvContent
	if dnsConfig := config.GetDnsConfig(); dnsConfig != nil {
		resolvContent, err = parseDNSOptions(dnsConfig.Servers, dnsConfig.Searches, dnsConfig.Options)
		if err != nil {
			return fmt.Errorf("failed to parse sandbox DNSConfig %+v: %v", dnsConfig, err)
		}
	}
	resolvPath := getResolvPath(rootDir)
	// 如果在配置中指定了dns，即resolvContent不为""，则将其写入resolvPath，否则直接将宿主机的/etc/resolv.conf写入
	if resolvContent == "" {
		// copy host's resolv.conf to resolvPath
		// resolvConfPath为"/etc/resolv.conf"
		err = c.os.CopyFile(resolvConfPath, resolvPath, 0644)
		if err != nil {
			return fmt.Errorf("failed to copy host's resolv.conf to %q: %v", resolvPath, err)
		}
	} else {
		err = c.os.WriteFile(resolvPath, []byte(resolvContent), 0644)
		if err != nil {
			return fmt.Errorf("failed to write resolv content to %q: %v", resolvPath, err)
		}
	}

	// Setup sandbox /dev/shm.
	// 设置sandbox的/dev/shm
	if config.GetLinux().GetSecurityContext().GetNamespaceOptions().GetHostIpc() {
		if _, err := c.os.Stat(devShm); err != nil {
			return fmt.Errorf("host %q is not available for host ipc: %v", devShm, err)
		}
	} else {
		sandboxDevShm := getSandboxDevShm(rootDir)
		if err := c.os.MkdirAll(sandboxDevShm, 0700); err != nil {
			return fmt.Errorf("failed to create sandbox shm: %v", err)
		}
		// defaultShmSize为64M
		shmproperty := fmt.Sprintf("mode=1777,size=%d", defaultShmSize)
		if err := c.os.Mount("shm", sandboxDevShm, "tmpfs", uintptr(unix.MS_NOEXEC|unix.MS_NOSUID|unix.MS_NODEV), shmproperty); err != nil {
			return fmt.Errorf("failed to mount sandbox shm: %v", err)
		}
	}

	return nil
}

// parseDNSOptions parse DNS options into resolv.conf format content,
// if none option is specified, will return empty with no error.
func parseDNSOptions(servers, searches, options []string) (string, error) {
	resolvContent := ""

	if len(searches) > maxDNSSearches {
		return "", fmt.Errorf("DNSOption.Searches has more than 6 domains")
	}

	if len(searches) > 0 {
		resolvContent += fmt.Sprintf("search %s\n", strings.Join(searches, " "))
	}

	if len(servers) > 0 {
		resolvContent += fmt.Sprintf("nameserver %s\n", strings.Join(servers, "\nnameserver "))
	}

	if len(options) > 0 {
		resolvContent += fmt.Sprintf("options %s\n", strings.Join(options, " "))
	}

	return resolvContent, nil
}

// unmountSandboxFiles unmount some sandbox files, we rely on the removal of sandbox root directory to
// remove these files. Unmount should *NOT* return error when:
//  1) The mount point is already unmounted.
//  2) The mount point doesn't exist.
func (c *criContainerdService) unmountSandboxFiles(rootDir string, config *runtime.PodSandboxConfig) error {
	if !config.GetLinux().GetSecurityContext().GetNamespaceOptions().GetHostIpc() {
		if err := c.os.Unmount(getSandboxDevShm(rootDir), unix.MNT_DETACH); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// toCNIPortMappings converts CRI port mappings to CNI.
func toCNIPortMappings(criPortMappings []*runtime.PortMapping) []ocicni.PortMapping {
	var portMappings []ocicni.PortMapping
	for _, mapping := range criPortMappings {
		if mapping.HostPort <= 0 {
			continue
		}
		portMappings = append(portMappings, ocicni.PortMapping{
			HostPort:      mapping.HostPort,
			ContainerPort: mapping.ContainerPort,
			Protocol:      strings.ToLower(mapping.Protocol.String()),
			HostIP:        mapping.HostIp,
		})
	}
	return portMappings
}
