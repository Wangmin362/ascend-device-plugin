/*
 * Copyright 2024 The HAMi Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package server

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"path"
	"time"

	"github.com/Project-HAMi/HAMi/pkg/device/ascend"
	"github.com/Project-HAMi/HAMi/pkg/util"
	"github.com/Project-HAMi/HAMi/pkg/util/nodelock"
	"github.com/Project-HAMi/ascend-device-plugin/internal/manager"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/klog/v2"
	"k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

const (
	// RegisterAnnos = "hami.io/node-register-ascend"
	// PodAllocAnno = "huawei.com/AscendDevices"
	NodeLockAscend = "hami.io/mutex.lock"
)

var (
	reportTimeOffset = flag.Int64("report_time_offset", 1, "report time offset")
)

type PluginServer struct {
	nodeName      string // 当前所在的节点名
	registerAnno  string // 注册到节点上的设备，volcano从这个注解上获取设备信息
	handshakeAnno string // 握手信息
	allocAnno     string // 给Pod分配设备之后，使用的注解
	grpcServer    *grpc.Server
	mgr           *manager.AscendManager
	socket        string
	stopCh        chan interface{}
	healthCh      chan int32
}

/*
	hami.io/node-handshake-Ascend910B: Requesting_2025-07-10 07:48:33
    hami.io/node-register-Ascend910B: '[
{"id":"Ascend910B-0","count":4,"devmem":65536,"devcore":20,"type":"Ascend910B","health":true},
{"index":1,"id":"Ascend910B-1","count":4,"devmem":65536,"devcore":20,"type":"Ascend910B","health":true},
{"index":2,"id":"Ascend910B-2","count":4,"devmem":65536,"devcore":20,"type":"Ascend910B","health":true},
{"index":3,"id":"Ascend910B-3","count":4,"devmem":65536,"devcore":20,"type":"Ascend910B","health":true},
{"index":4,"id":"Ascend910B-4","count":4,"devmem":65536,"devcore":20,"type":"Ascend910B","health":true},
{"index":5,"id":"Ascend910B-5","count":4,"devmem":65536,"devcore":20,"type":"Ascend910B","health":true},
{"index":6,"id":"Ascend910B-6","count":4,"devmem":65536,"devcore":20,"type":"Ascend910B","health":true},
{"index":7,"id":"Ascend910B-7","count":4,"devmem":65536,"devcore":20,"type":"Ascend910B","health":true}]'
*/

func NewPluginServer(mgr *manager.AscendManager, nodeName string) (*PluginServer, error) {
	return &PluginServer{
		nodeName:      nodeName,
		registerAnno:  fmt.Sprintf("hami.io/node-register-%s", mgr.CommonWord()),
		handshakeAnno: fmt.Sprintf("hami.io/node-handshake-%s", mgr.CommonWord()),
		allocAnno:     fmt.Sprintf("huawei.com/%s", mgr.CommonWord()),
		grpcServer:    grpc.NewServer(),
		mgr:           mgr,
		// TODO 这里只上报了一种类型的资源， 为什么不考虑整卡资源和虚卡资源分开上报？
		socket:   path.Join(v1beta1.DevicePluginPath, fmt.Sprintf("%s.sock", mgr.CommonWord())),
		stopCh:   make(chan interface{}),
		healthCh: make(chan int32),
	}, nil
}

func (ps *PluginServer) Start() error {
	ps.stopCh = make(chan interface{})
	// 通过查询驱动获取当前节点所有芯片的信息，包括物理ID、逻辑ID、UUID、内存、AI核心，健康状态等信息
	err := ps.mgr.UpdateDevice()
	if err != nil {
		return err
	}
	// 1. 启动DP，并等待DP启动成功
	// 2. 移除之前注册的socket文件，然后重新启动GRPC服务，此时会重新创建socket文件
	err = ps.serve()
	if err != nil {
		return err
	}
	// 注册kubelet
	err = ps.registerKubelet()
	if err != nil {
		return err
	}
	// 定时获取设备的健康状态，上报到Kubelet。与此同时定期更新节点的注解【设备】信息以及握手信息
	go ps.watchAndRegister()
	return nil
}

func (ps *PluginServer) Stop() error {
	close(ps.stopCh)
	ps.grpcServer.Stop()
	return nil
}

func (ps *PluginServer) dial(unixSocketPath string, timeout time.Duration) (*grpc.ClientConn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c, err := grpc.DialContext(ctx, unixSocketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithContextDialer(func(ctx2 context.Context, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx2, "unix", addr)
		}),
	)

	if err != nil {
		return nil, err
	}
	return c, nil
}

// 移除之前注册的socket文件，然后重新启动GRPC服务，此时会重新创建socket文件
func (ps *PluginServer) serve() error {
	// 移除之前的/var/lib/kubelet/device-plugins/Ascend910B.sock文件，因为这里需要重新向kubelet注册
	_ = os.Remove(ps.socket)
	sock, err := net.Listen("unix", ps.socket)
	if err != nil {
		return err
	}
	// 驱动GRPC服务之前，注册GRPC的服务，有点类似于注册路由的感觉
	v1beta1.RegisterDevicePluginServer(ps.grpcServer, ps)
	// 获取当前的资源名，譬如huawei.com/Ascend910A， huawei.com/Ascend910B等等
	resourceName := ps.mgr.ResourceName()
	go func() {
		lastCrashTime := time.Now()
		restartCount := 0
		for {
			klog.Infof("Starting GRPC server for '%s'", resourceName)
			// 启动GRPC服务，GRPC服务，必须要在注册kubelet之前就启动，否则一会kubelet回调ListAndWatch方法的时候,会调用失败
			err := ps.grpcServer.Serve(sock)
			if err == nil {
				break
			}

			klog.Infof("GRPC server for '%s' crashed with error: %v", resourceName, err)

			// restart if it has not been too often
			// i.e. if server has crashed more than 5 times and it didn't last more than one hour each time
			if restartCount > 5 {
				// quit
				klog.Fatalf("GRPC server for '%s' has repeatedly crashed recently. Quitting", resourceName)
			}
			timeSinceLastCrash := time.Since(lastCrashTime).Seconds()
			lastCrashTime = time.Now()
			if timeSinceLastCrash > 3600 {
				// it has been one hour since the last crash.. reset the count
				// to reflect on the frequency
				restartCount = 1
			} else {
				restartCount++
			}
		}
	}()

	// Wait for server to start by launching a blocking connexion
	// 等待GRPC服务启动完成
	conn, err := ps.dial(ps.socket, 5*time.Second)
	if err != nil {
		return err
	}
	_ = conn.Close()

	return nil
}

func (ps *PluginServer) registerKubelet() error {
	conn, err := ps.dial(v1beta1.KubeletSocket, 5*time.Second)
	if err != nil {
		return err
	}
	defer func(conn *grpc.ClientConn) {
		_ = conn.Close()
	}(conn)
	client := v1beta1.NewRegistrationClient(conn)
	reqt := &v1beta1.RegisterRequest{
		Version:      v1beta1.Version,
		Endpoint:     path.Base(ps.socket),
		ResourceName: ps.mgr.ResourceName(),
		Options: &v1beta1.DevicePluginOptions{
			GetPreferredAllocationAvailable: false,
		},
	}

	_, err = client.Register(context.Background(), reqt)
	if err != nil {
		return err
	}
	return nil
}

// 所谓注册HAMI其实就是给节点打上hami相关的注解
func (ps *PluginServer) registerHAMi() error {
	// 获取所有的设备
	devs := ps.mgr.GetDevices()
	apiDevices := make([]*util.DeviceInfo, 0, len(devs))
	// hami currently believes that the index starts from 0 and is continuous.
	for i, dev := range devs {
		apiDevices = append(apiDevices, &util.DeviceInfo{
			Index:   uint(i),
			ID:      dev.UUID,
			Count:   int32(ps.mgr.VDeviceCount()), // 昇腾的算力切分，本质上就是应用昇腾的模板，因此这里最多可以创建的虚卡数量为可分配内存处于最小模板需要使用的内存大小
			Devmem:  int32(dev.Memory),
			Devcore: dev.AICore,
			Type:    ps.mgr.CommonWord(),
			Numa:    0,
			Health:  dev.Health,
		})
	}
	annos := make(map[string]string)
	// 向节点注册设备信息
	annos[ps.registerAnno] = util.MarshalNodeDevices(apiDevices)
	// 向节点更新握手信息
	annos[ps.handshakeAnno] = "Reported_" + time.Now().Add(time.Duration(*reportTimeOffset)*time.Second).Format("2006.01.02 15:04:05")
	node, err := util.GetNode(ps.nodeName)
	if err != nil {
		return fmt.Errorf("get node %s error: %v", ps.nodeName, err)
	}
	err = util.PatchNodeAnnotations(node, annos)
	if err != nil {
		return fmt.Errorf("patch node %s annotations error: %v", ps.nodeName, err)
	}
	klog.V(5).Infof("patch node %s annotations: %v", ps.nodeName, annos)
	return nil
}

// 定时获取设备的健康状态，上报到Kubelet。与此同时定期更新节点的注解【设备】信息以及握手信息
func (ps *PluginServer) watchAndRegister() {
	timer := time.After(1 * time.Second)
	for {
		select {
		case <-ps.stopCh:
			klog.Infof("stop watch and register")
			return
		case <-timer:
		}
		unhealthy := ps.mgr.GetUnHealthIDs()
		if len(unhealthy) > 0 {
			if err := ps.mgr.UpdateDevice(); err != nil {
				klog.Errorf("update device error: %v", err)
				timer = time.After(5 * time.Second)
				continue
			}
			ps.healthCh <- unhealthy[0]
		}
		// 所谓注册HAMI其实就是给节点打上hami相关的注解，一个是更新节点设备信息，一个是更新握手信息
		err := ps.registerHAMi()
		if err != nil {
			klog.Errorf("register HAMi error: %v", err)
			timer = time.After(5 * time.Second)
		} else {
			klog.V(3).Infof("register HAMi success")
			timer = time.After(30 * time.Second)
		}
	}
}

// 调度器调度完成之后，会把分配的设备写入到Pod注解中，这里在解析注解信息获取当前Pod分配到的设备以及对应的模板
func (ps *PluginServer) parsePodAnnotation(pod *v1.Pod) ([]int32, []string, error) {
	// 从调度其中获取当前分配的卡和模板
	anno, ok := pod.Annotations[ps.allocAnno]
	if !ok {
		return nil, nil, fmt.Errorf("annotation %s not set", "huawei.com/Ascend")
	}
	var rtInfo []ascend.RuntimeInfo
	err := json.Unmarshal([]byte(anno), &rtInfo)
	if err != nil {
		return nil, nil, fmt.Errorf("annotation %s value %s invalid", ps.allocAnno, anno)
	}
	var IDs []int32
	var temps []string
	for _, info := range rtInfo {
		if info.UUID == "" {
			continue
		}
		// 通过UUID找到对应的昇腾设备
		d := ps.mgr.GetDeviceByUUID(info.UUID)
		if d == nil {
			return nil, nil, fmt.Errorf("unknown uuid: %s", info.UUID)
		}
		IDs = append(IDs, d.PhyID)
		temps = append(temps, info.Temp)
	}
	if len(IDs) == 0 {
		return nil, nil, fmt.Errorf("annotation %s value %s invalid", ps.allocAnno, anno)
	}
	return IDs, temps, nil
}

func (ps *PluginServer) apiDevices() []*v1beta1.Device {
	devs := ps.mgr.GetDevices()
	devices := make([]*v1beta1.Device, 0, len(devs))
	vCount := ps.mgr.VDeviceCount()
	for _, dev := range devs {
		health := v1beta1.Unhealthy
		if dev.Health {
			health = v1beta1.Healthy
		}
		// TODO 这里会不会有问题，因为一张卡可能不是等分的，有可能是多种规格的组合
		// TODO 这里的写法类似于TimeSlice的写法， 就是给调度器上报多个资源，这样就可以复用一张卡
		for i := 0; i < vCount; i++ {
			device := v1beta1.Device{
				ID:     fmt.Sprintf("%s-%d", dev.UUID, i),
				Health: health,
			}
			devices = append(devices, &device)
		}
	}
	klog.V(5).Infof("api devices: %v", devices)
	return devices
}

func (ps *PluginServer) GetDevicePluginOptions(context.Context, *v1beta1.Empty) (*v1beta1.DevicePluginOptions, error) {
	return &v1beta1.DevicePluginOptions{}, nil
}

func (ps *PluginServer) ListAndWatch(e *v1beta1.Empty, s v1beta1.DevicePlugin_ListAndWatchServer) error {
	_ = s.Send(&v1beta1.ListAndWatchResponse{Devices: ps.apiDevices()})
	for {
		select {
		case <-ps.stopCh:
			return nil
		case <-ps.healthCh:
			// 当前设备的状态发生了变化，因此通知一下Kubelet
			_ = s.Send(&v1beta1.ListAndWatchResponse{Devices: ps.apiDevices()})
		}
	}
}

func (ps *PluginServer) GetPreferredAllocation(context.Context, *v1beta1.PreferredAllocationRequest) (*v1beta1.PreferredAllocationResponse, error) {
	return nil, fmt.Errorf("not supported")
}

func (ps *PluginServer) Allocate(ctx context.Context, reqs *v1beta1.AllocateRequest) (*v1beta1.AllocateResponse, error) {
	klog.V(5).Infof("Allocate: %v", reqs)
	// 通过节点锁获取当前节点处于Pending的Pod，volcano调度之后会给当前节点设置一把锁，锁信息中会包含当前需要分配设备的Pod信息 ns/name
	pod, err := util.GetPendingPod(ctx, ps.nodeName)
	if err != nil {
		klog.Errorf("get pending pod error: %v", err)
		// 分配失败，直接释放锁
		lockerr := nodelock.ReleaseNodeLock(ps.nodeName, NodeLockAscend, pod, false)
		if lockerr != nil {
			klog.Errorf("failed to release lock:%s", err.Error())
		}
		return nil, fmt.Errorf("get pending pod error: %v", err)
	}
	resp := v1beta1.ContainerAllocateResponse{}
	// 调度器调度完成之后，会把分配的设备写入到Pod注解中，这里在解析注解信息获取当前Pod分配到的设备以及对应的模板
	IDs, temps, err := ps.parsePodAnnotation(pod)
	if err != nil {
		lockerr := nodelock.ReleaseNodeLock(ps.nodeName, NodeLockAscend, pod, false)
		if lockerr != nil {
			klog.Errorf("failed to release lock:%s", err.Error())
		}
		return nil, fmt.Errorf("parse pod annotation error: %v", err)
	}
	if len(IDs) == 0 {
		lockerr := nodelock.ReleaseNodeLock(ps.nodeName, NodeLockAscend, pod, false)
		if lockerr != nil {
			klog.Errorf("failed to release lock:%s", err.Error())
		}
		return nil, fmt.Errorf("empty id from pod annotation")
	}
	ascendVisibleDevices := fmt.Sprintf("%d", IDs[0])
	ascendVNPUSpec := ""
	// 本质上是拼接都好，Q: 为啥不直接使用strings.join(IDS, ","), A：因为这里是拼接字符串，而不是拼接数字
	for i := 1; i < len(IDs); i++ {
		ascendVisibleDevices = fmt.Sprintf("%s,%d", ascendVisibleDevices, IDs[i])
	}
	// 1. 遍历模板，找到第一个模板直接退出
	// 2. 如果只多卡的情况下，昇腾并不支持模板，想使用模板，只能分配一张虚拟卡，其实hami webhook也会检查这种情况，不要内需申请多张虚拟卡
	for i := 0; i < len(temps); i++ {
		if temps[i] != "" {
			ascendVNPUSpec = temps[i]
			break
		}
	}
	resp.Envs = make(map[string]string)
	resp.Envs["ASCEND_VISIBLE_DEVICES"] = ascendVisibleDevices
	if ascendVNPUSpec != "" {
		resp.Envs["ASCEND_VNPU_SPECS"] = ascendVNPUSpec
	}
	klog.V(5).Infof("allocate response: %v", resp)
	lockerr := nodelock.ReleaseNodeLock(ps.nodeName, NodeLockAscend, pod, true)
	if lockerr != nil {
		klog.Errorf("failed to release lock:%s", err.Error())
	}
	return &v1beta1.AllocateResponse{ContainerResponses: []*v1beta1.ContainerAllocateResponse{&resp}}, nil
}

func (ps *PluginServer) PreStartContainer(context.Context, *v1beta1.PreStartContainerRequest) (*v1beta1.PreStartContainerResponse, error) {
	return &v1beta1.PreStartContainerResponse{}, nil
}
