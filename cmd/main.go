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

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"syscall"

	"github.com/Project-HAMi/ascend-device-plugin/internal"
	"github.com/Project-HAMi/ascend-device-plugin/internal/manager"
	"github.com/Project-HAMi/ascend-device-plugin/internal/server"
	"github.com/Project-HAMi/ascend-device-plugin/version"
	"github.com/fsnotify/fsnotify"
	"huawei.com/npu-exporter/v6/common-utils/hwlog"
	"k8s.io/klog/v2"
	"k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

/* config-file配置文件如下
apiVersion: v1
data:
  ascend-config.yaml: |-
    vnpus:
    - chipName: 910B
      commonWord: Ascend910A
      resourceName: huawei.com/Ascend910A
      resourceMemoryName: huawei.com/Ascend910A-memory
      memoryAllocatable: 32768
      memoryCapacity: 32768
      aiCore: 30
      templates:
        - name: vir02
          memory: 2184
          aiCore: 2
        - name: vir04
          memory: 4369
          aiCore: 4
        - name: vir08
          memory: 8738
          aiCore: 8
        - name: vir16
          memory: 17476
          aiCore: 16
    - chipName: 910B2
      commonWord: Ascend910B2
      resourceName: huawei.com/Ascend910B2
      resourceMemoryName: huawei.com/Ascend910B2-memory
      memoryAllocatable: 65536
      memoryCapacity: 65536
      aiCore: 24
      aiCPU: 6
      topologyPairs:
        - 1,2,3,4,5,6,7
        - 0,2,3,4,5,6,7
        - 0,1,3,4,5,6,7
        - 0,1,2,4,5,6,7
        - 0,1,2,3,5,6,7
        - 0,1,2,3,4,6,7
        - 0,1,2,3,4,5,7
        - 0,1,2,3,4,5,6
      templates:
        - name: vir03_1c_8g
          memory: 8192
          aiCore: 3
          aiCPU: 1
        - name: vir06_1c_16g
          memory: 16384
          aiCore: 6
          aiCPU: 1
        - name: vir12_3c_32g
          memory: 32768
          aiCore: 12
          aiCPU: 3
    - chipName: 910B3
      commonWord: Ascend910B
      resourceName: huawei.com/Ascend910B
      resourceMemoryName: huawei.com/Ascend910B-memory
      memoryAllocatable: 65536
      memoryCapacity: 65536
      aiCore: 20
      aiCPU: 7
      topologyPairs:
        - 1,2,3,4,5,6,7
        - 0,2,3,4,5,6,7
        - 0,1,3,4,5,6,7
        - 0,1,2,4,5,6,7
        - 0,1,2,3,5,6,7
        - 0,1,2,3,4,6,7
        - 0,1,2,3,4,5,7
        - 0,1,2,3,4,5,6
      templates:
        - name: vir05_1c_16g
          memory: 16384
          aiCore: 5
          aiCPU: 1
        - name: vir10_3c_32g
          memory: 32768
          aiCore: 10
          aiCPU: 3
    - chipName: 910B4
      commonWord: Ascend910B4
      resourceName: huawei.com/Ascend910B4
      resourceMemoryName: huawei.com/Ascend910B4-memory
      memoryAllocatable: 32768
      memoryCapacity: 32768
      aiCore: 20
      aiCPU: 7
      templates:
        - name: vir05_1c_8g
          memory: 8192
          aiCore: 5
          aiCPU: 1
        - name: vir10_3c_16g
          memory: 16384
          aiCore: 10
          aiCPU: 3
    - chipName: 910B4-1
      commonWord: Ascend910B4
      resourceName: huawei.com/Ascend910B4
      resourceMemoryName: huawei.com/Ascend910B4-memory
      memoryAllocatable: 65536
      memoryCapacity: 65536
      aiCore: 20
      aiCPU: 7
      templates:
        - name: vir05_1c_8g
          memory: 8192
          aiCore: 5
          aiCPU: 1
        - name: vir10_3c_16g
          memory: 16384
          aiCore: 10
          aiCPU: 3
    - chipName: 310P3
      commonWord: Ascend310P
      resourceName: huawei.com/Ascend310P
      resourceMemoryName: huawei.com/Ascend310P-memory
      memoryAllocatable: 21527
      memoryCapacity: 24576
      aiCore: 8
      aiCPU: 7
      templates:
        - name: vir01
          memory: 3072
          aiCore: 1
          aiCPU: 1
        - name: vir02
          memory: 6144
          aiCore: 2
          aiCPU: 2
        - name: vir04
          memory: 12288
          aiCore: 4
          aiCPU: 4
kind: ConfigMap
metadata:
  name: hami-device-plugin-ascend
  namespace: rise-vast-system
*/

var (
	hwLoglevel = flag.Int("hw_loglevel", 0, "huawei log level, -1-debug, 0-info, 1-warning, 2-error 3-critical default value: 0")
	configFile = flag.String("config_file", "", "config file path")
	nodeName   = flag.String("node_name", os.Getenv("NODE_NAME"), "node name")
)

func checkFlags() {
	version.CheckVersionFlag()
	if *configFile == "" {
		klog.Fatalf("config file not set, use --config_file to set config file path")
	}
	if *nodeName == "" {
		klog.Fatalf("node name not set, use --node_name or env NODE_NAME to set node name")
	}
}

func start(ps *server.PluginServer) error {
	klog.Info("Starting FS watcher.")
	// 监听/var/lib/kubelet/device-plugins目录，当kubelet重启时，会重新创建该目录
	watcher, err := internal.NewFSWatcher(v1beta1.DevicePluginPath)
	if err != nil {
		return fmt.Errorf("failed to create FS watcher: %v", err)
	}
	defer func(watcher *fsnotify.Watcher) {
		_ = watcher.Close()
	}(watcher)

	klog.Info("Starting OS watcher.")
	sigs := internal.NewOSWatcher(syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	var restarting bool
	//var restartTimeout <-chan time.Time
restart:
	if restarting {
		err := ps.Stop()
		if err != nil {
			klog.Errorf("Failed to stop plugin server: %v", err)
		}
	}
	restarting = true
	klog.Info("Starting Plugins.")
	err = ps.Start()
	if err != nil {
		klog.Errorf("Failed to start plugin server: %v", err)
		return err
	}

	for {
		select {
		//case <-restartTimeout:
		//	goto restart
		case event := <-watcher.Events:
			if event.Name == v1beta1.KubeletSocket && event.Op&fsnotify.Create == fsnotify.Create {
				klog.Infof("inotify: %s created, restarting.", v1beta1.KubeletSocket)
				goto restart
			}
		case err := <-watcher.Errors:
			klog.Errorf("inotify: %s", err)
		case s := <-sigs:
			switch s {
			case syscall.SIGHUP:
				klog.Info("Received SIGHUP, restarting.")
				goto restart
			default:
				klog.Infof("Received signal \"%v\", shutting down.", s)
				goto exit
			}
		}
	}
exit:
	err = ps.Stop()
	if err != nil {
		klog.Errorf("Failed to stop plugin server: %v", err)
		return err
	}
	return nil
}

func main() {
	klog.InitFlags(nil)
	flag.Parse()
	checkFlags()
	klog.Infof("version: %s", version.GetVersion())
	// TODO 这里最好把这个配置文件打印出来，方便定位问题
	klog.Infof("using config file: %s", *configFile)
	config := &hwlog.LogConfig{
		OnlyToStdout: true,
		LogLevel:     *hwLoglevel,
	}
	err := hwlog.InitRunLogger(config, context.Background())
	if err != nil {
		klog.Fatalf("init huawei run logger failed, %v", err)
	}
	// 这里的AscendManager本质上其实就是昇腾DeviceManager的封装, 拥有DCMI接口，因此可以调用底层驱动获取芯片信息
	mgr, err := manager.NewAscendManager()
	if err != nil {
		klog.Fatalf("init AscendManager failed, error is %v", err)
	}
	// 通过驱动获取当前节点芯片的配置信息，通过芯片的名字找到当前芯片的配置，并对当前芯片的虚拟化模板按照从小到大的顺序排序
	err = mgr.LoadConfig(*configFile)
	if err != nil {
		klog.Fatalf("load config failed, error is %v", err)
	}
	server, err := server.NewPluginServer(mgr, *nodeName)
	if err != nil {
		klog.Fatalf("init PluginServer failed, error is %v", err)
	}

	err = start(server)
	if err != nil {
		klog.Fatalf("start PluginServer failed, error is %v", err)
	}
}
