/*
Copyright 2022 The Numaproj Authors.

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

package scaling

import (
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru"
	"go.uber.org/zap"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dfv1 "github.com/numaproj/numaflow/pkg/apis/numaflow/v1alpha1"
	daemonclient "github.com/numaproj/numaflow/pkg/daemon/client"
	"github.com/numaproj/numaflow/pkg/isb"
	"github.com/numaproj/numaflow/pkg/shared/logging"
)

type Scaler struct {
	client    client.Client
	vertexMap map[string]*list.Element
	// List of the vertex namespaced name, format is "namespace/name"
	vertexList *list.List
	lock       *sync.RWMutex
	options    *options
	// Cache to store the vertex metrics such as pending message number
	vertexMetricsCache *lru.Cache
	daemonClientsCache *lru.Cache
}

// NewScaler returns a Scaler instance.
func NewScaler(client client.Client, opts ...Option) *Scaler {
	scalerOpts := defaultOptions()
	for _, opt := range opts {
		if opt != nil {
			opt(scalerOpts)
		}
	}
	s := &Scaler{
		client:     client,
		options:    scalerOpts,
		vertexMap:  make(map[string]*list.Element),
		vertexList: list.New(),
		lock:       new(sync.RWMutex),
	}
	// cache top 100 daemon clients
	s.daemonClientsCache, _ = lru.NewWithEvict(s.options.clientsCacheSize, func(key, value interface{}) {
		_ = value.(*daemonclient.DaemonClient).Close()
	})
	vertexMetricsCache, _ := lru.New(10000)
	s.vertexMetricsCache = vertexMetricsCache
	return s
}

// Contains returns if the Scaler contains the key.
func (s *Scaler) Contains(key string) bool {
	s.lock.RLock()
	defer s.lock.RUnlock()
	_, ok := s.vertexMap[key]
	return ok
}

// Length returns how many vertices are being watched for autoscaling
func (s *Scaler) Length() int {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return s.vertexList.Len()
}

// StartWatching put a key (namespace/name) into the Scaler
func (s *Scaler) StartWatching(key string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	if _, ok := s.vertexMap[key]; !ok {
		s.vertexMap[key] = s.vertexList.PushBack(key)
	}
}

// StopWatching stops autoscaling on the key (namespace/name)
func (s *Scaler) StopWatching(key string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	if e, ok := s.vertexMap[key]; ok {
		_ = s.vertexList.Remove(e)
		delete(s.vertexMap, key)
	}
}

// Function scale() defines each of the worker's job.
// It waits for keys in the channel, and starts a scaling job
func (s *Scaler) scale(ctx context.Context, id int, keyCh <-chan string) {
	log := logging.FromContext(ctx)
	log.Infof("Started autoscaling worker %v", id)
	for {
		select {
		case <-ctx.Done():
			log.Infof("Stopped scaling worker %v", id)
			return
		case key := <-keyCh:
			if err := s.scaleOneVertex(ctx, key, id); err != nil {
				log.Errorw("Failed to scale a vertex", zap.String("vertex", key), zap.Error(err))
			}
		}
	}
}

// scaleOneVertex implements the detailed logic of scaling up/down a vertex.
//
// For source vertices which have both rate and pending message information,
// if there are multiple partitions, we consider the max desired replicas among all partitions.
//
//	desiredReplicas = currentReplicas * pending / (targetProcessingTime * rate)
//
// For UDF and sinks which have the read buffer information
//
//	singleReplicaContribution = (totalAvailableBufferLength - pending) / currentReplicas
//	desiredReplicas = targetAvailableBufferLength / singleReplicaContribution
//
// Back pressure factor
// When desiredReplicas > currentReplicas:
// If there's back pressure in the directly connected vertices, desiredReplicas = currentReplicas-1;
// If there's back pressure in the downstream vertices (not connected), desiredReplicas remains the same.
func (s *Scaler) scaleOneVertex(ctx context.Context, key string, worker int) error {
	log := logging.FromContext(ctx).With("worker", fmt.Sprint(worker)).With("vertexKey", key)
	log.Debugf("Working on key: %s", key)
	strs := strings.Split(key, "/")
	if len(strs) != 2 {
		return fmt.Errorf("invalid key %q", key)
	}
	namespace := strs[0]
	vertexFullName := strs[1]
	vertex := &dfv1.Vertex{}
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: vertexFullName}, vertex); err != nil {
		if apierrors.IsNotFound(err) {
			s.StopWatching(key)
			log.Info("No corresponding Vertex found, stopped watching")
			return nil
		}
		return fmt.Errorf("failed to query vertex object of key %q, %w", key, err)
	}
	if !vertex.GetDeletionTimestamp().IsZero() {
		s.StopWatching(key)
		log.Debug("Vertex being deleted")
		return nil
	}
	if !vertex.Scalable() { // A vertex which is not scalable, such as HTTP source, or autoscaling disabled.
		s.StopWatching(key) // Remove it in case it's watched.
		return nil
	}
	if time.Since(vertex.Status.LastScaledAt.Time).Seconds() < float64(vertex.Spec.Scale.GetCooldownSeconds()) {
		log.Debug("Cooldown period, skip scaling.")
		return nil
	}
	if vertex.Status.Phase != dfv1.VertexPhaseRunning {
		log.Debug("Vertex not in Running phase.")
		return nil
	}
	pl := &dfv1.Pipeline{}
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: vertex.Spec.PipelineName}, pl); err != nil {
		if apierrors.IsNotFound(err) {
			s.StopWatching(key)
			log.Info("No corresponding Pipeline found, stopped watching")
			return nil
		}
		return fmt.Errorf("failed to query Pipeline object of key %q, %w", key, err)
	}
	if !pl.GetDeletionTimestamp().IsZero() {
		s.StopWatching(key)
		log.Debug("Corresponding Pipeline being deleted")
		return nil
	}
	if pl.Spec.Lifecycle.GetDesiredPhase() != dfv1.PipelinePhaseRunning {
		log.Debug("Corresponding Pipeline not in Running state")
		return nil
	}
	if int(vertex.Status.Replicas) != vertex.GetReplicas() {
		log.Debugf("Vertex %s might be under processing, replicas mismatch", vertex.Name)
		return nil
	}
	if vertex.Status.Replicas == 0 { // Was scaled to 0
		if seconds := time.Since(vertex.Status.LastScaledAt.Time).Seconds(); seconds >= float64(vertex.Spec.Scale.GetZeroReplicaSleepSeconds()) {
			log.Debugf("Vertex %s has slept %v seconds, scaling up to peek", vertex.Name, seconds)
			return s.patchVertexReplicas(ctx, vertex, 1)
		} else {
			log.Debugf("Vertex %q has slept %v seconds, hasn't reached zeroReplicaSleepSeconds (%v seconds)", vertex.Name, seconds, vertex.Spec.Scale.GetZeroReplicaSleepSeconds())
			return nil
		}
	}
	var err error
	dClient, _ := s.daemonClientsCache.Get(pl.GetDaemonServiceURL())
	if dClient == nil {
		dClient, err = daemonclient.NewDaemonServiceClient(pl.GetDaemonServiceURL())
		if err != nil {
			return fmt.Errorf("failed to get daemon service client for pipeline %s, %w", pl.Name, err)
		}
		s.daemonClientsCache.Add(pl.GetDaemonServiceURL(), dClient)
	}

	daemonClient := dClient.(*daemonclient.DaemonClient)
	vMetrics, err := daemonClient.GetVertexMetrics(ctx, pl.Name, vertex.Spec.Name)
	if err != nil {
		return fmt.Errorf("failed to get metrics of vertex key %q, %w", key, err)
	}
	// Avg rate and pending for autoscaling are both in the map with key "default", see "pkg/metrics/metrics.go".
	// vMetrics is a map which contains metrics of all the partitions of a vertex.
	// We need to aggregate them to get the total rate and pending of the vertex.
	// If any of the partition doesn't have the rate or pending information, we skip scaling.
	// we need both aggregated and partition level metrics for scaling, because we use aggregated metrics to
	// determine whether we can scale down to 0 and for calculating back pressure, and partition level metrics to determine
	// the max desired replicas among all the partitions.
	partitionRates := make([]float64, 0)
	partitionPending := make([]int64, 0)
	totalRate := float64(0)
	totalPending := int64(0)
	for _, m := range vMetrics {
		rate, existing := m.ProcessingRates["default"]
		// If rate is not available, we skip scaling.
		if !existing || rate < 0 { // Rate not available
			log.Debugf("Vertex %s has no rate information, skip scaling", vertex.Name)
			return nil
		}
		partitionRates = append(partitionRates, rate)
		totalRate += rate

		pending, existing := m.Pendings["default"]
		if !existing || pending < 0 || pending == isb.PendingNotAvailable {
			// Pending not available, we don't do anything
			log.Debugf("Vertex %s has no pending messages information, skip scaling", vertex.Name)
			return nil
		}
		totalPending += pending
		partitionPending = append(partitionPending, pending)
	}

	// Add pending information to cache for back pressure calculation, if there is a backpressure it will impact all the partitions.
	// So we only need to add the total pending to the cache.
	_ = s.vertexMetricsCache.Add(key+"/pending", totalPending)
	partitionBufferLengths := make([]int64, 0)
	partitionAvailableBufferLengths := make([]int64, 0)
	totalBufferLength := int64(0)
	targetAvailableBufferLength := int64(0)
	if !vertex.IsASource() { // Only non-source vertex has buffer to read
		for _, bufferName := range vertex.OwnedBuffers() {
			if bInfo, err := daemonClient.GetPipelineBuffer(ctx, pl.Name, bufferName); err != nil {
				return fmt.Errorf("failed to get the read buffer information of vertex %q, %w", vertex.Name, err)
			} else {
				if bInfo.BufferLength == nil || bInfo.BufferUsageLimit == nil {
					return fmt.Errorf("invalid read buffer information of vertex %q, length or usage limit is missing", vertex.Name)
				}
				partitionBufferLengths = append(partitionBufferLengths, int64(float64(bInfo.GetBufferLength())*bInfo.GetBufferUsageLimit()))
				partitionAvailableBufferLengths = append(partitionAvailableBufferLengths, int64(float64(bInfo.GetBufferLength())*float64(vertex.Spec.Scale.GetTargetBufferAvailability())/100))
				totalBufferLength += int64(float64(*bInfo.BufferLength) * *bInfo.BufferUsageLimit)
				targetAvailableBufferLength += int64(float64(*bInfo.BufferLength) * float64(vertex.Spec.Scale.GetTargetBufferAvailability()) / 100)
				// Add to cache for back pressure calculation
			}
		}
		// Add processing rate information to cache for back pressure calculation
		_ = s.vertexMetricsCache.Add(key+"/length", totalBufferLength)
	}
	var desired int32
	current := int32(vertex.GetReplicas())
	// if both totalRate and totalPending are 0, we scale down to 0
	// since pending contains the pending acks, we can scale down to 0.
	if totalPending == 0 && totalRate == 0 {
		desired = 0
	} else {
		desired = s.desiredReplicas(ctx, vertex, partitionRates, partitionPending, partitionBufferLengths, partitionAvailableBufferLengths)
	}
	log.Debugf("Calculated desired replica number of vertex %q is: %d", vertex.Name, desired)
	max := vertex.Spec.Scale.GetMaxReplicas()
	min := vertex.Spec.Scale.GetMinReplicas()
	if desired > max {
		desired = max
		log.Debugf("Calculated desired replica number %d of vertex %q is greater than max, using max %d", vertex.Name, desired, max)
	}
	if desired < min {
		desired = min
		log.Debugf("Calculated desired replica number %d of vertex %q is smaller than min, using min %d", vertex.Name, desired, min)
	}
	if current > max || current < min { // Someone might have manually scaled up/down the vertex
		return s.patchVertexReplicas(ctx, vertex, desired)
	}
	maxAllowed := int32(vertex.Spec.Scale.GetReplicasPerScale())
	if desired < current {
		diff := current - desired
		if diff > maxAllowed {
			diff = maxAllowed
		}
		return s.patchVertexReplicas(ctx, vertex, current-diff) // We scale down gradually
	}
	if desired > current {
		// When scaling up, need to check back pressure
		directPressure, downstreamPressure := s.hasBackPressure(*pl, *vertex)
		if directPressure {
			if current > 1 {
				log.Debugf("Vertex %s has direct back pressure from connected vertices, decreasing one replica", key)
				return s.patchVertexReplicas(ctx, vertex, current-1)
			} else {
				log.Debugf("Vertex %s has direct back pressure from connected vertices, skip scaling", key)
				return nil
			}
		} else if downstreamPressure {
			log.Debugf("Vertex %s has back pressure in downstream vertices, skip scaling", key)
			return nil
		}
		diff := desired - current
		if diff > maxAllowed {
			diff = maxAllowed
		}
		return s.patchVertexReplicas(ctx, vertex, current+diff) // We scale up gradually
	}
	return nil
}

func (s *Scaler) desiredReplicas(ctx context.Context, vertex *dfv1.Vertex, partitionProcessingRate []float64, partitionPending []int64, partitionBufferLengths []int64, partitionAvailableBufferLengths []int64) int32 {
	maxDesired := int32(1)
	// We calculate the max desired replicas based on the pending messages and processing rate for each partition.
	for i := 0; i < len(partitionPending); i++ {
		rate := partitionProcessingRate[i]
		pending := partitionPending[i]
		var desired int32
		if pending == 0 || rate == 0 {
			// Pending is 0 and rate is not 0, or rate is 0 and pending is not 0, we don't do anything.
			// Technically this would not happen because the pending includes ackpending, which means rate and pending are either both 0, or both > 0.
			// But we still keep this check here for safety.
			// in this case, we don't update the desired replicas because we don't know how many replicas are needed.
			// we cannot go with current replicas because ideally we should scale down when pending is 0 or rate is 0.
			continue
		}
		if vertex.IsASource() {
			// For sources, we calculate the time of finishing processing the pending messages,
			// and then we know how many replicas are needed to get them done in target seconds.
			desired = int32(math.Round(((float64(pending) / rate) / float64(vertex.Spec.Scale.GetTargetProcessingSeconds())) * float64(vertex.Status.Replicas)))
		} else {
			// For UDF and sinks, we calculate the available buffer length, and consider it is the contribution of current replicas,
			// then we figure out how many replicas are needed to keep the available buffer length at target level.
			if pending >= partitionBufferLengths[i] {
				// Simply return current replica number + max allowed if the pending messages are more than available buffer length
				desired = int32(vertex.Status.Replicas) + int32(vertex.Spec.Scale.GetReplicasPerScale())
			} else {
				singleReplicaContribution := float64(partitionBufferLengths[i]-pending) / float64(vertex.Status.Replicas)
				desired = int32(math.Round(float64(partitionAvailableBufferLengths[i]) / singleReplicaContribution))
			}
		}
		// we only scale down to zero when the total pending and total rate are both zero.
		if desired == 0 {
			desired = 1
		}
		if desired > int32(pending) { // For some corner cases, we don't want to scale up to more than pending.
			desired = int32(pending)
		}
		// maxDesired is the max of all partitions
		if desired > maxDesired {
			maxDesired = desired
		}
	}
	return maxDesired
}

// Start function starts the autoscaling worker group.
// Each worker keeps picking up scaling tasks (which contains vertex keys) to calculate the desired replicas,
// and patch the vertex spec with the new replica number if needed.
func (s *Scaler) Start(ctx context.Context) error {
	log := logging.FromContext(ctx).Named("autoscaler")
	log.Info("Starting autoscaler...")
	keyCh := make(chan string)
	ctx, cancel := context.WithCancel(logging.WithLogger(ctx, log))
	defer cancel()
	// Worker group
	for i := 1; i <= s.options.workers; i++ {
		go s.scale(ctx, i, keyCh)
	}

	// Function assign() moves an element in the list from the front to the back,
	// and send to the channel so that it can be picked up by a worker.
	assign := func() {
		s.lock.Lock()
		defer s.lock.Unlock()
		if s.vertexList.Len() == 0 {
			return
		}
		e := s.vertexList.Front()
		if key, ok := e.Value.(string); ok {
			s.vertexList.MoveToBack(e)
			keyCh <- key
		}
	}

	// Following for loop keeps calling assign() function to assign scaling tasks to the workers.
	// It makes sure each element in the list will be assigned every N milliseconds.
	for {
		select {
		case <-ctx.Done():
			log.Info("Shutting down scaling job assigner")
			// clear the daemon clients cache
			s.daemonClientsCache.Purge()
			return nil
		default:
			assign()
		}
		// Make sure each of the key will be assigned at most every N milliseconds.
		time.Sleep(time.Millisecond * time.Duration(func() int {
			l := s.Length()
			if l == 0 {
				return s.options.taskInterval
			}
			result := s.options.taskInterval / l
			if result > 0 {
				return result
			}
			return 1
		}()))
	}
}

// hasBackPressure checks if there's back pressure in the downstream buffers
// It returns if 2 bool values, which represent:
// 1. If there's back pressure in the connected vertices;
// 2. If there's back pressure in any of the downstream vertices.
func (s *Scaler) hasBackPressure(pl dfv1.Pipeline, vertex dfv1.Vertex) (bool, bool) {
	downstreamEdges := pl.GetDownstreamEdges(vertex.Spec.Name)
	directPressure, downstreamPressure := false, false
loop:
	for _, e := range downstreamEdges {
		vertexKey := pl.Namespace + "/" + pl.Name + "-" + e.To
		pendingVal, ok := s.vertexMetricsCache.Get(vertexKey + "/pending")
		if !ok { // Vertex key has not been cached, skip it.
			continue
		}
		pending := pendingVal.(int64)
		bufferLengthVal, ok := s.vertexMetricsCache.Get(vertexKey + "/length")
		if !ok { // Buffer length has not been cached, skip it.
			continue
		}
		length := bufferLengthVal.(int64)
		if float64(pending)/float64(length) >= s.options.backPressureThreshold {
			downstreamPressure = true
			if e.From == vertex.Spec.Name {
				directPressure = true
				break loop
			}
		}
	}
	return directPressure, downstreamPressure
}

func (s *Scaler) patchVertexReplicas(ctx context.Context, vertex *dfv1.Vertex, desiredReplicas int32) error {
	log := logging.FromContext(ctx)
	origin := vertex.Spec.Replicas
	vertex.Spec.Replicas = pointer.Int32(desiredReplicas)
	body, err := json.Marshal(vertex)
	if err != nil {
		return fmt.Errorf("failed to marshal vertex object to json, %w", err)
	}
	if err := s.client.Patch(ctx, vertex, client.RawPatch(types.MergePatchType, body)); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to patch vertex replicas, %w", err)
	}
	log.Infow("Auto scaling - vertex replicas changed", zap.Int32p("from", origin), zap.Int32("to", desiredReplicas), zap.String("pipeline", vertex.Spec.PipelineName), zap.String("vertex", vertex.Spec.Name))
	return nil
}

// KeyOfVertex returns the unique key of a vertex
func KeyOfVertex(vertex dfv1.Vertex) string {
	return fmt.Sprintf("%s/%s", vertex.Namespace, vertex.Name)
}
