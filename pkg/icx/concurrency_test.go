// SPDX-License-Identifier: Apache-2.0
package icx_test

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/icx"
)

// TestRecallAndWalk_MassiveConcurrencyRace tests 60 concurrent workers
// performing simultaneous Remember, RecallScoped (with ribbon format), AddASTNode, AddASTEdge,
// MultihopWalk (with ribbon format), and ResetSession under go test -race.
func TestRecallAndWalk_MassiveConcurrencyRace(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	lat := icx.NewLocalLattice()
	spaceID := "stress_concurrency_space"

	// Pre-seed some facts and AST nodes
	for i := 0; i < 30; i++ {
		_, err := lat.Remember(ctx, icx.RememberRequest{
			Text:     fmt.Sprintf("seed fact %d for stress test with function Handle_%d", i, i),
			SourceID: fmt.Sprintf("file_%d.go", i),
			Family:   "core",
			SpaceID:  spaceID,
		})
		if err != nil {
			t.Fatalf("seed Remember failed: %v", err)
		}

		nodeID := fmt.Sprintf("ast_node_%d", i)
		lat.AddASTNode(&icx.LatticeNode{
			ID:        nodeID,
			Name:      nodeID,
			Type:      "function",
			FilePath:  fmt.Sprintf("pkg/service/file_%d.go", i),
			Dimension: "3D_VOLUMETRIC",
		})
		if i > 0 {
			lat.AddASTEdge(fmt.Sprintf("ast_node_%d", i-1), "calls", nodeID)
		}
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 500)
	const numWorkers = 60
	const opsPerWorker = 50

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(workerID * 7919)))

			for op := 0; op < opsPerWorker; op++ {
				select {
				case <-ctx.Done():
					return
				default:
				}

				action := rng.Intn(5)
				switch action {
				case 0:
					// Remember new fact
					_, err := lat.Remember(ctx, icx.RememberRequest{
						Text:     fmt.Sprintf("dynamic fact w%d op%d with keywords alpha beta gamma", workerID, op),
						SourceID: fmt.Sprintf("w%d_file.go", workerID),
						Family:   "stress",
						SpaceID:  spaceID,
					})
					if err != nil {
						errCh <- fmt.Errorf("worker %d: Remember failed: %w", workerID, err)
						return
					}

				case 1:
					// RecallScoped with format ribbon
					query := "alpha"
					if op%2 == 0 {
						query = "Handle"
					}
					resp, err := lat.RecallScoped(ctx, icx.RecallRequest{
						Query:   query,
						SpaceID: spaceID,
						TopK:    5,
						Format:  icx.FormatRibbon,
					})
					if err != nil {
						errCh <- fmt.Errorf("worker %d: RecallScoped failed: %w", workerID, err)
						return
					}
					if resp.Ribbon == "" {
						errCh <- fmt.Errorf("worker %d: RecallScoped ribbon is empty", workerID)
						return
					}
					if !strings.HasPrefix(resp.Ribbon, "<nav:ribbon:") || !strings.HasSuffix(resp.Ribbon, "/>") {
						errCh <- fmt.Errorf("worker %d: invalid ribbon: %s", workerID, resp.Ribbon)
						return
					}

				case 2:
					// Mutate AST graph and MultihopWalk
					dynID := fmt.Sprintf("w%d_node_%d", workerID, op)
					lat.AddASTNode(&icx.LatticeNode{
						ID:        dynID,
						Name:      dynID,
						Type:      "type",
						Dimension: "3D_VOLUMETRIC",
					})
					lat.AddASTEdge(dynID, "calls", fmt.Sprintf("ast_node_%d", rng.Intn(30)))

					walkResp, err := lat.MultihopWalk(ctx, icx.MultihopWalkRequest{
						StartNode: dynID,
						MaxHops:   4,
						SpaceID:   spaceID,
						Format:    icx.FormatRibbon,
					})
					if err != nil {
						errCh <- fmt.Errorf("worker %d: MultihopWalk failed: %w", workerID, err)
						return
					}
					if !walkResp.OK {
						errCh <- fmt.Errorf("worker %d: MultihopWalk returned OK=false: %s", workerID, walkResp.Error)
						return
					}
					if walkResp.Ribbon == "" {
						errCh <- fmt.Errorf("worker %d: MultihopWalk ribbon is empty", workerID)
						return
					}

				case 3:
					// CallTool dispatch with ribbon format
					tool := "icx_recall_scoped"
					args := map[string]any{
						"query":    "Handle",
						"space_id": spaceID,
						"format":   "ribbon",
					}
					if op%2 == 1 {
						tool = "icx_multihop_walk"
						args = map[string]any{
							"start_node": fmt.Sprintf("ast_node_%d", rng.Intn(30)),
							"format":     "ribbon",
						}
					}
					callRes, err := lat.CallTool(ctx, tool, args)
					if err != nil {
						errCh <- fmt.Errorf("worker %d: CallTool %s failed: %w", workerID, tool, err)
						return
					}
					if callRes.IsError {
						errCh <- fmt.Errorf("worker %d: CallTool %s returned error: %v", workerID, tool, callRes.Content)
						return
					}

				case 4:
					// Occasional session reset on a separate sub-space
					if op%20 == 0 {
						_, err := lat.ResetSession(ctx, fmt.Sprintf("sub_space_%d", workerID))
						if err != nil {
							errCh <- fmt.Errorf("worker %d: ResetSession failed: %w", workerID, err)
							return
						}
					}
				}
			}
		}(w)
	}

	wg.Wait()
	close(errCh)

	var errList []error
	for err := range errCh {
		errList = append(errList, err)
	}
	if len(errList) > 0 {
		t.Fatalf("encountered %d concurrency race errors, first: %v", len(errList), errList[0])
	}
}
