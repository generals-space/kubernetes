// +build linux

/*
Copyright 2015 The Kubernetes Authors.

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

package oom

import (
	"fmt"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog"

	"github.com/google/cadvisor/utils/oomparser"
)

// 监听系统中的 OOM 事件(通过解析 /dev/kmsg 内容实现此功能)
type realWatcher struct {
	recorder record.EventRecorder
}

var _ Watcher = &realWatcher{}

// NewWatcher creates and initializes a OOMWatcher based on parameters.
func NewWatcher(recorder record.EventRecorder) Watcher {
	return &realWatcher{
		recorder: recorder,
	}
}

const systemOOMEvent = "SystemOOM"

// Start 开始监听系统中的 OOM 事件(通过解析 /dev/kmsg 内容实现此功能)
// 关于 /dev/kmsg 文件与 dmesg 命令的区别, 可以见如下文章
// [内核调试 /proc/kmsg 和 dmesg](https://blog.csdn.net/zlcchina/article/details/24195331)
// Start watches for system oom's and 
// records an event for every system oom encountered.
func (ow *realWatcher) Start(ref *v1.ObjectReference) error {
	oomLog, err := oomparser.New()
	if err != nil {
		return err
	}
	outStream := make(chan *oomparser.OomInstance, 10)
	go oomLog.StreamOoms(outStream)

	go func() {
		defer runtime.HandleCrash()

		for event := range outStream {
			if event.ContainerName == "/" {
				klog.V(1).Infof("Got sys oom event: %v", event)
				eventMsg := "System OOM encountered"
				if event.ProcessName != "" && event.Pid != 0 {
					eventMsg = fmt.Sprintf(
						"%s, victim process: %s, pid: %d", 
						eventMsg, 
						event.ProcessName, 
						event.Pid,
					)
				}
				ow.recorder.PastEventf(
					ref, 
					metav1.Time{Time: event.TimeOfDeath}, 
					v1.EventTypeWarning, 
					systemOOMEvent, 
					eventMsg,
				)
			}
		}
		klog.Errorf("Unexpectedly stopped receiving OOM notifications")
	}()
	return nil
}
