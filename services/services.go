/*
   Copyright The containerd Authors.

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

package services

const (
	// ContentService is id of content service.
	ContentService = "content-service" //具体指的是什么
	// SnapshotsService is id of snapshots service.
	SnapshotsService = "snapshots-service"
	// ImagesService is id of images service.
	ImagesService = "images-service" // 容器镜像的镜像？
	// ContainersService is id of containers service.
	ContainersService = "containers-service" //非运行状态的容器服务
	// TasksService is id of tasks service.
	TasksService = "tasks-service" //运行状态的服务
	// NamespacesService is id of namespaces service.
	NamespacesService = "namespaces-service"
	// LeasesService is id of leases service.
	LeasesService = "leases-service" //租约服务？
	// DiffService is id of diff service.
	DiffService = "diff-service" //差异服务？
)
