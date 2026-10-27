// SPDX-License-Identifier: Apache-2.0
// Package icx implements 5-hop simplicial causal AST walking across
// calls, implements, imports, and defines with cycle detection.

package icx

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ASTEdge represents a directed relational edge in the simplicial AST graph.
type ASTEdge struct {
	SourceID string `json:"source_id"`
	Relation string `json:"relation"` // "calls", "implements", "imports", "defines"
	TargetID string `json:"target_id"`
}

// AddASTNode inserts a 3D volumetric node into the lattice graph.
func (lat *LocalMemoryCache) AddASTNode(node *LatticeNode) {
	lat.mu.Lock()
	defer lat.mu.Unlock()
	if node.Dimension == "" {
		node.Dimension = "3D_VOLUMETRIC"
	}
	lat.nodes3D[node.ID] = node
}

// AddASTEdge inserts a directed edge between two AST nodes.
func (lat *LocalMemoryCache) AddASTEdge(src, rel, dst string) {
	lat.mu.Lock()
	defer lat.mu.Unlock()
	lat.addEdgeLocked(src, rel, dst)
}

func (lat *LocalMemoryCache) addEdgeLocked(src, rel, dst string) {
	lat.edges[src] = append(lat.edges[src], ASTEdge{SourceID: src, Relation: rel, TargetID: dst})
	lat.revEdges[dst] = append(lat.revEdges[dst], ASTEdge{SourceID: src, Relation: rel, TargetID: dst})
}

// isAncestor checks whether target is an ancestor of curr along the current search path.
// This identifies true back-edges in directed/undirected traversals while distinguishing
// them from cross-edges/reconvergent edges in Directed Acyclic Graphs (DAGs, e.g. diamond patterns).
func isAncestor(curr, target string, parentMap map[string]string) bool {
	if curr == target {
		return true // self-loop
	}
	p := parentMap[curr]
	for p != "" {
		if p == target {
			return true
		}
		p = parentMap[p]
	}
	return false
}

// ----------------------------------------------------------------------------
// Certified Tool 7: icx_multihop_walk
// 5-hop simplicial causal AST walk across calls, implements, imports, defines
// with cycle detection.
// ----------------------------------------------------------------------------
func (lat *LocalMemoryCache) MultihopWalk(ctx context.Context, req MultihopWalkRequest) (*MultihopWalkResponse, error) {
	startNS := time.Now()
	lat.mu.RLock()
	defer lat.mu.RUnlock()

	maxHops := req.MaxHops
	if maxHops <= 0 {
		maxHops = MaxHopsDefault
	}
	if maxHops > 10 {
		maxHops = 10
	}

	startNode := req.StartNode
	if _, exists := lat.nodes3D[startNode]; !exists {
		// Attempt resolution by exact Name match across 3D nodes (e.g. symbol "Button" or "UserProps")
		for id, node := range lat.nodes3D {
			if node.Name == startNode {
				startNode = id
				break
			}
		}
	}
	if _, exists := lat.nodes3D[startNode]; !exists {
		fnCandidate := fmt.Sprintf("func_%s", startNode)
		if len(lat.edges[fnCandidate]) > 0 || len(lat.revEdges[fnCandidate]) > 0 {
			startNode = fnCandidate
		}
	}
	if _, exists := lat.nodes3D[startNode]; !exists {
		if len(lat.edges[startNode]) == 0 && len(lat.revEdges[startNode]) == 0 {
			return &MultihopWalkResponse{
				OK:           false,
				Start:        startNode,
				StartNode:    startNode,
				MaxHops:      maxHops,
				LatencyUs:    float64(time.Since(startNS).Microseconds()),
				LatencyMs:    float64(time.Since(startNS).Microseconds()) / 1000.0,
				Error:        "NODE_NOT_FOUND",
			}, nil
		}
	}

	type queueItem struct {
		nodeID   string
		depth    int
		relation string
		parentID string
	}

	visited := make(map[string]bool)
	parentMap := make(map[string]string) // child -> parent for path reconstruction
	queue := []queueItem{{nodeID: startNode, depth: 0, relation: "", parentID: ""}}
	visited[startNode] = true

	var steps []WalkStep
	var nodesList []any
	cyclesDetected := 0
	deepestNode := startNode
	maxDepthReached := 0

	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]

		// Extract node metadata if available
		nodeInfo := lat.nodes3D[curr.nodeID]
		name, nodeType, filePath, line := curr.nodeID, "unknown", "", 0
		if nodeInfo != nil {
			name = nodeInfo.Name
			nodeType = nodeInfo.Type
			filePath = nodeInfo.FilePath
			line = nodeInfo.Line
		}

		step := WalkStep{
			NodeID:   curr.nodeID,
			Name:     name,
			Type:     nodeType,
			FilePath: filePath,
			Line:     line,
			Relation: curr.relation,
			Depth:    curr.depth,
			ParentID: curr.parentID,
		}
		steps = append(steps, step)
		nodesList = append(nodesList, map[string]any{
			"node_id":  curr.nodeID,
			"name":     name,
			"type":     nodeType,
			"relation": curr.relation,
			"depth":    curr.depth,
		})

		if curr.depth > maxDepthReached {
			maxDepthReached = curr.depth
			deepestNode = curr.nodeID
		}

		if curr.depth >= maxHops {
			continue
		}

		// Collect outgoing candidate edges
		var candidateEdges []ASTEdge
		for _, edge := range lat.edges[curr.nodeID] {
			if req.Relation == "" || req.Relation == "all" || edge.Relation == req.Relation {
				candidateEdges = append(candidateEdges, edge)
			}
		}

		for _, edge := range candidateEdges {
			target := edge.TargetID
			if visited[target] {
				// Only flag as a cycle if the target is an ancestor on the current search path (true back-edge).
				// Reconvergent edges in DAGs (cross-edges, such as diamond patterns A->B->D and A->C->D)
				// are not cycles.
				if isAncestor(curr.nodeID, target, parentMap) {
					cyclesDetected++
				}
				continue
			}
			visited[target] = true
			parentMap[target] = curr.nodeID
			queue = append(queue, queueItem{
				nodeID:   target,
				depth:    curr.depth + 1,
				relation: edge.Relation,
				parentID: curr.nodeID,
			})
		}
	}

	// Reconstruct longest path from startNode to deepestNode
	var path []string
	cur := deepestNode
	for cur != "" {
		path = append([]string{cur}, path...)
		if cur == startNode {
			break
		}
		cur = parentMap[cur]
	}

	dur := time.Since(startNS)
	lat.telemetry.Walks.Add(1)

	var ribbonStr string
	if req.Format == FormatRibbon || req.Format == "ribbon" {
		ribbonStr = FormatWalkRibbon(startNode, steps)
	}

	return &MultihopWalkResponse{
		OK:             true,
		Start:          startNode,
		StartNode:      startNode,
		Relation:       req.Relation,
		MaxHops:        maxHops,
		Hops:           maxDepthReached,
		NodesReached:   len(steps),
		Path:           path,
		Visited:        len(visited),
		VisitedCount:   len(visited),
		CyclesDetected: cyclesDetected,
		Steps:          steps,
		Nodes:          nodesList,
		Ribbon:         ribbonStr,
		LatencyUs:      float64(dur.Microseconds()),
		LatencyMs:      float64(dur.Microseconds()) / 1000.0,
	}, nil
}

// FormatWalkRibbon formats walk steps into a dense 1D geodesic ribbon representation.
func FormatWalkRibbon(startNode string, steps []WalkStep) string {
	var sb strings.Builder
	root := strings.TrimPrefix(startNode, "file_")
	if root == "" {
		root = "root"
	}
	sb.WriteString(fmt.Sprintf("<nav:ribbon: root=%q hops=5 | ", root))
	if len(steps) == 0 {
		sb.WriteString("isolated />")
		return sb.String()
	}
	items := make([]string, 0, len(steps))
	seen := make(map[string]bool)
	for _, step := range steps {
		key := fmt.Sprintf("%d:%s:%s", step.Depth, step.Name, step.Type)
		if seen[key] {
			continue
		}
		seen[key] = true
		rel := step.Relation
		if rel == "" {
			if step.Depth == 0 {
				rel = "root"
			} else {
				rel = "node"
			}
		}
		items = append(items, fmt.Sprintf("d%d:%s(%s):%s", step.Depth, step.Name, step.Type, rel))
	}
	sb.WriteString(strings.Join(items, " > "))
	sb.WriteString(" />")
	return sb.String()
}

// IngestGoAST parses Go source code and builds 3D AST nodes and relations.
func (lat *LocalMemoryCache) IngestGoAST(filePath string, src []byte) error {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filePath, src, parser.ParseComments)
	if err != nil {
		return err
	}

	lat.mu.Lock()
	defer lat.mu.Unlock()

	fileID := fmt.Sprintf("file_%s", filePath)
	lat.nodes3D[fileID] = &LatticeNode{
		ID:        fileID,
		Dimension: "3D_VOLUMETRIC",
		Type:      "file",
		Name:      filePath,
		FilePath:  filePath,
		Timestamp: time.Now().Unix(),
	}

	// 1. Imports
	for _, imp := range f.Imports {
		pkgPath := strings.Trim(imp.Path.Value, `"`)
		pkgID := fmt.Sprintf("pkg_%s", pkgPath)
		lat.addEdgeLocked(fileID, "imports", pkgID)
	}

	// 2. Types and Functions
	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.TypeSpec:
			pos := fset.Position(node.Pos())
			typeID := fmt.Sprintf("type_%s_%s", filePath, node.Name.Name)
			kind := "type"
			switch node.Type.(type) {
			case *ast.StructType:
				kind = "struct"
			case *ast.InterfaceType:
				kind = "interface"
			}
			lat.nodes3D[typeID] = &LatticeNode{
				ID:        typeID,
				Dimension: "3D_VOLUMETRIC",
				Type:      kind,
				Name:      node.Name.Name,
				FilePath:  filePath,
				Line:      pos.Line,
				Timestamp: time.Now().Unix(),
			}
			lat.addEdgeLocked(fileID, "defines", typeID)

		case *ast.FuncDecl:
			pos := fset.Position(node.Pos())
			funcID := fmt.Sprintf("func_%s_%s", filePath, node.Name.Name)
			lat.nodes3D[funcID] = &LatticeNode{
				ID:        funcID,
				Dimension: "3D_VOLUMETRIC",
				Type:      "function",
				Name:      node.Name.Name,
				FilePath:  filePath,
				Line:      pos.Line,
				Timestamp: time.Now().Unix(),
			}
			lat.addEdgeLocked(fileID, "defines", funcID)

			// Inspect function body for calls
			if node.Body != nil {
				ast.Inspect(node.Body, func(bodyNode ast.Node) bool {
					if call, ok := bodyNode.(*ast.CallExpr); ok {
						switch fn := call.Fun.(type) {
						case *ast.Ident:
							calleeID := fmt.Sprintf("func_%s", fn.Name)
							lat.addEdgeLocked(funcID, "calls", calleeID)
						case *ast.SelectorExpr:
							calleeID := fmt.Sprintf("func_%s", fn.Sel.Name)
							lat.addEdgeLocked(funcID, "calls", calleeID)
						}
					}
					return true
				})
			}
		}
		return true
	})

	return nil
}

// IngestPolyglotAST parses source code across Go, Python, JavaScript/TypeScript, and HTML,
// building 3D simplicial AST nodes and relational edges in the lattice.
func (lat *LocalMemoryCache) IngestPolyglotAST(filePath string, src []byte) error {
	ext := strings.ToLower(filepath.Ext(filePath))
	switch ext {
	case ".go":
		return lat.IngestGoAST(filePath, src)
	case ".py":
		return lat.ingestPythonAST(filePath, src)
	case ".js", ".ts", ".jsx", ".tsx":
		return lat.ingestJavaScriptAST(filePath, src)
	case ".html", ".htm":
		return lat.ingestHTMLAST(filePath, src)
	case ".rs":
		return lat.ingestRustAST(filePath, src)
	case ".c", ".cpp", ".cc", ".cxx", ".h", ".hpp":
		return lat.ingestCppAST(filePath, src)
	case ".java":
		return lat.ingestJavaAST(filePath, src)
	case ".sql":
		return lat.ingestSqlAST(filePath, src)
	default:
		lat.mu.Lock()
		defer lat.mu.Unlock()
		fileID := fmt.Sprintf("file_%s", filePath)
		lat.nodes3D[fileID] = &LatticeNode{
			ID:        fileID,
			Dimension: "3D_VOLUMETRIC",
			Type:      "file",
			Name:      filePath,
			FilePath:  filePath,
			Timestamp: time.Now().Unix(),
		}
		return nil
	}
}

func (lat *LocalMemoryCache) ingestPythonAST(filePath string, src []byte) error {
	lat.mu.Lock()
	defer lat.mu.Unlock()

	fileID := fmt.Sprintf("file_%s", filePath)
	lat.nodes3D[fileID] = &LatticeNode{
		ID:        fileID,
		Dimension: "3D_VOLUMETRIC",
		Type:      "file",
		Name:      filePath,
		FilePath:  filePath,
		Timestamp: time.Now().Unix(),
	}

	lines := strings.Split(string(src), "\n")
	var currentFuncID string
	var lastDecorators []string

	importRe := regexp.MustCompile(`^\s*(?:import\s+([a-zA-Z0-9_.,\s]+)|from\s+([a-zA-Z0-9_.]+)\s+import\s+([a-zA-Z0-9_.,\s*]+))`)
	classRe := regexp.MustCompile(`^\s*class\s+([a-zA-Z0-9_]+)(?:\(([^)]*)\))?:`)
	funcRe := regexp.MustCompile(`^\s*(?:async\s+)?def\s+([a-zA-Z0-9_]+)\s*\(([^)]*)\)`)
	decoratorRe := regexp.MustCompile(`^\s*@([a-zA-Z0-9_.]+)(?:\((.*)\))?`)
	callRe := regexp.MustCompile(`([a-zA-Z0-9_]+)\s*\(`)

	for lineIdx, line := range lines {
		lineNum := lineIdx + 1
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		// Decorator
		if m := decoratorRe.FindStringSubmatch(trimmed); len(m) > 1 {
			lastDecorators = append(lastDecorators, m[1])
			continue
		}

		// Imports
		if m := importRe.FindStringSubmatch(trimmed); len(m) > 0 {
			if m[1] != "" {
				pkgs := strings.Split(m[1], ",")
				for _, p := range pkgs {
					p = strings.TrimSpace(p)
					if p != "" {
						pkgParts := strings.Fields(p)
						pkgName := pkgParts[0]
						pkgID := fmt.Sprintf("pkg_%s", pkgName)
						lat.addEdgeLocked(fileID, "imports", pkgID)
					}
				}
			} else if m[2] != "" {
				pkgID := fmt.Sprintf("pkg_%s", m[2])
				lat.addEdgeLocked(fileID, "imports", pkgID)
			}
			lastDecorators = nil
			continue
		}

		// Classes
		if m := classRe.FindStringSubmatch(trimmed); len(m) > 1 {
			className := m[1]
			classID := fmt.Sprintf("type_%s_%s", filePath, className)
			lat.nodes3D[classID] = &LatticeNode{
				ID:        classID,
				Dimension: "3D_VOLUMETRIC",
				Type:      "class",
				Name:      className,
				FilePath:  filePath,
				Line:      lineNum,
				Timestamp: time.Now().Unix(),
			}
			lat.addEdgeLocked(fileID, "defines", classID)
			currentFuncID = ""
			lastDecorators = nil
			continue
		}

		// Functions / Methods
		if m := funcRe.FindStringSubmatch(trimmed); len(m) > 1 {
			funcName := m[1]
			funcID := fmt.Sprintf("func_%s_%s", filePath, funcName)
			lat.nodes3D[funcID] = &LatticeNode{
				ID:        funcID,
				Dimension: "3D_VOLUMETRIC",
				Type:      "function",
				Name:      funcName,
				FilePath:  filePath,
				Line:      lineNum,
				Timestamp: time.Now().Unix(),
			}
			lat.addEdgeLocked(fileID, "defines", funcID)
			currentFuncID = funcID

			// Wire route decorators like @app.get(...), @app.post(...)
			for _, dec := range lastDecorators {
				if strings.Contains(dec, "app.get") || strings.Contains(dec, "app.post") ||
					strings.Contains(dec, "app.route") || strings.Contains(dec, "router.") {
					routeID := fmt.Sprintf("route_%s_%s", filePath, dec)
					lat.nodes3D[routeID] = &LatticeNode{
						ID:        routeID,
						Dimension: "3D_VOLUMETRIC",
						Type:      "route",
						Name:      dec,
						FilePath:  filePath,
						Line:      lineNum,
						Timestamp: time.Now().Unix(),
					}
					lat.addEdgeLocked(funcID, "routes", routeID)
				}
			}
			lastDecorators = nil
			continue
		}

		// Function calls inside function body
		if currentFuncID != "" {
			matches := callRe.FindAllStringSubmatch(trimmed, -1)
			for _, cm := range matches {
				if len(cm) > 1 {
					callee := cm[1]
					if callee != "def" && callee != "class" && callee != "if" && callee != "for" && callee != "while" && callee != "print" && callee != "len" {
						calleeID := fmt.Sprintf("func_%s", callee)
						lat.addEdgeLocked(currentFuncID, "calls", calleeID)
					}
				}
			}
		}

		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			currentFuncID = ""
			lastDecorators = nil
		}
	}

	return nil
}

func (lat *LocalMemoryCache) ingestJavaScriptAST(filePath string, src []byte) error {
	lat.mu.Lock()
	defer lat.mu.Unlock()

	fileID := fmt.Sprintf("file_%s", filePath)
	lat.nodes3D[fileID] = &LatticeNode{
		ID:        fileID,
		Dimension: "3D_VOLUMETRIC",
		Type:      "file",
		Name:      filePath,
		FilePath:  filePath,
		Timestamp: time.Now().Unix(),
	}

	lines := strings.Split(string(src), "\n")
	importRe := regexp.MustCompile(`(?m)^\s*import\s+(?:(?:(?:\{[^}]*\}|\*\s+as\s+[a-zA-Z0-9_]+|[a-zA-Z0-9_]+)\s*,\s*)?(?:\{[^}]*\}|\*\s+as\s+[a-zA-Z0-9_]+|[a-zA-Z0-9_]+)\s+from\s+)?['"]([^'"]+)['"]`)
	requireRe := regexp.MustCompile(`require\(['"]([^'"]+)['"]\)`)
	ifaceRe := regexp.MustCompile(`(?:export\s+)?interface\s+([a-zA-Z0-9_]+)(?:\s+extends\s+([a-zA-Z0-9_,\s]+))?`)
	typeAliasRe := regexp.MustCompile(`(?:export\s+)?type\s+([a-zA-Z0-9_]+)\s*(?:<[^>]*>)?\s*=`)
	enumRe := regexp.MustCompile(`(?:export\s+)?enum\s+([a-zA-Z0-9_]+)`)
	classRe := regexp.MustCompile(`(?:export\s+)?(?:default\s+)?class\s+([a-zA-Z0-9_]+)(?:\s+extends\s+([a-zA-Z0-9_]+))?(?:\s+implements\s+([a-zA-Z0-9_,\s]+))?`)
	methodRe := regexp.MustCompile(`^\s*(?:public|private|protected|static|async)?\s*([a-zA-Z0-9_]+)\s*\([^)]*\)\s*(?::\s*[^;{]+)?\s*\{`)
	funcRe := regexp.MustCompile(`(?:export\s+)?(?:default\s+)?function\s+([a-zA-Z0-9_]+)\s*(?:<[^>]*>)?\s*\(`)
	arrowRe := regexp.MustCompile(`(?:export\s+)?(?:const|let|var)\s+([a-zA-Z0-9_]+)(?:\s*:\s*React\.(?:FC|FunctionComponent)(?:<[^>]*>)?)?\s*=\s*(?:async\s+)?(?:\([^)]*\)|[a-zA-Z0-9_]+)\s*(?::\s*[^=]+)?\s*=>`)
	routeRe := regexp.MustCompile(`(?:app|router)\.(get|post|put|delete|patch)\(\s*['"]([^'"]+)['"]`)
	jsxCompRe := regexp.MustCompile(`<([A-Z][a-zA-Z0-9_]*)[\s/>]`)
	callRe := regexp.MustCompile(`\b([a-zA-Z0-9_]+)\s*\(`)

	reservedKeywords := map[string]bool{
		"if": true, "for": true, "while": true, "switch": true, "catch": true,
		"require": true, "import": true, "function": true, "return": true, "super": true,
		"console": true, "typeof": true, "sizeof": true, "throw": true, "new": true,
		"delete": true, "case": true, "default": true, "try": true, "finally": true,
		"constructor": true, "async": true, "await": true, "yield": true, "expect": true,
	}

	var currentScopeID string
	var currentClassID string

	for lineIdx, line := range lines {
		lineNum := lineIdx + 1
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "/*") || strings.HasPrefix(trimmed, "*") {
			continue
		}

		// 1. Imports
		if m := importRe.FindStringSubmatch(trimmed); len(m) > 1 {
			pkgID := fmt.Sprintf("pkg_%s", m[1])
			lat.addEdgeLocked(fileID, "imports", pkgID)
		}
		if m := requireRe.FindStringSubmatch(trimmed); len(m) > 1 {
			pkgID := fmt.Sprintf("pkg_%s", m[1])
			lat.addEdgeLocked(fileID, "imports", pkgID)
		}

		// 2. TypeScript Interfaces
		if m := ifaceRe.FindStringSubmatch(trimmed); len(m) > 1 {
			name := m[1]
			ifaceID := fmt.Sprintf("type_%s_%s", filePath, name)
			lat.nodes3D[ifaceID] = &LatticeNode{
				ID:        ifaceID,
				Dimension: "3D_VOLUMETRIC",
				Type:      "interface",
				Name:      name,
				FilePath:  filePath,
				Line:      lineNum,
				Timestamp: time.Now().Unix(),
			}
			lat.addEdgeLocked(fileID, "defines", ifaceID)
			// Connect base interfaces if extends clause present
			if len(m) > 2 && m[2] != "" {
				bases := strings.Split(m[2], ",")
				for _, b := range bases {
					bClean := strings.TrimSpace(b)
					if bClean != "" {
						baseID := fmt.Sprintf("type_%s", bClean)
						lat.addEdgeLocked(ifaceID, "implements", baseID)
					}
				}
			}
			currentScopeID = ifaceID
			continue
		}

		// 3. TypeScript Type Aliases
		if m := typeAliasRe.FindStringSubmatch(trimmed); len(m) > 1 {
			name := m[1]
			typeID := fmt.Sprintf("type_%s_%s", filePath, name)
			lat.nodes3D[typeID] = &LatticeNode{
				ID:        typeID,
				Dimension: "3D_VOLUMETRIC",
				Type:      "type",
				Name:      name,
				FilePath:  filePath,
				Line:      lineNum,
				Timestamp: time.Now().Unix(),
			}
			lat.addEdgeLocked(fileID, "defines", typeID)
			currentScopeID = typeID
			continue
		}

		// 4. TypeScript Enums
		if m := enumRe.FindStringSubmatch(trimmed); len(m) > 1 {
			name := m[1]
			enumID := fmt.Sprintf("type_%s_%s", filePath, name)
			lat.nodes3D[enumID] = &LatticeNode{
				ID:        enumID,
				Dimension: "3D_VOLUMETRIC",
				Type:      "enum",
				Name:      name,
				FilePath:  filePath,
				Line:      lineNum,
				Timestamp: time.Now().Unix(),
			}
			lat.addEdgeLocked(fileID, "defines", enumID)
			currentScopeID = enumID
			continue
		}

		// 5. Classes with extends & implements
		if m := classRe.FindStringSubmatch(trimmed); len(m) > 1 {
			name := m[1]
			classID := fmt.Sprintf("type_%s_%s", filePath, name)
			lat.nodes3D[classID] = &LatticeNode{
				ID:        classID,
				Dimension: "3D_VOLUMETRIC",
				Type:      "class",
				Name:      name,
				FilePath:  filePath,
				Line:      lineNum,
				Timestamp: time.Now().Unix(),
			}
			lat.addEdgeLocked(fileID, "defines", classID)
			currentClassID = classID
			currentScopeID = classID

			if len(m) > 2 && m[2] != "" {
				superID := fmt.Sprintf("type_%s", strings.TrimSpace(m[2]))
				lat.addEdgeLocked(classID, "implements", superID)
			}
			if len(m) > 3 && m[3] != "" {
				ifaces := strings.Split(m[3], ",")
				for _, iface := range ifaces {
					ifaceClean := strings.TrimSpace(iface)
					if ifaceClean != "" {
						ifaceID := fmt.Sprintf("type_%s", ifaceClean)
						lat.addEdgeLocked(classID, "implements", ifaceID)
					}
				}
			}
			continue
		}

		// 6. Functions & React Components
		var fnName string
		var isComponent bool

		if m := funcRe.FindStringSubmatch(trimmed); len(m) > 1 {
			fnName = m[1]
		} else if m := arrowRe.FindStringSubmatch(trimmed); len(m) > 1 {
			fnName = m[1]
			if strings.Contains(trimmed, "React.FC") || strings.Contains(trimmed, "FunctionComponent") {
				isComponent = true
			}
		}

		if fnName != "" {
			kind := "function"
			if isComponent || (len(fnName) > 0 && fnName[0] >= 'A' && fnName[0] <= 'Z') {
				kind = "component"
			}
			funcID := fmt.Sprintf("func_%s_%s", filePath, fnName)
			lat.nodes3D[funcID] = &LatticeNode{
				ID:        funcID,
				Dimension: "3D_VOLUMETRIC",
				Type:      kind,
				Name:      fnName,
				FilePath:  filePath,
				Line:      lineNum,
				Timestamp: time.Now().Unix(),
			}
			lat.addEdgeLocked(fileID, "defines", funcID)

			// Propagate existing references to/from short symbol name
			shortID := fmt.Sprintf("func_%s", fnName)
			lat.addEdgeLocked(funcID, "alias", shortID)
			lat.addEdgeLocked(shortID, "resolves_to", funcID)
			for _, edge := range lat.edges[shortID] {
				lat.addEdgeLocked(funcID, edge.Relation, edge.TargetID)
			}
			for _, edge := range lat.revEdges[shortID] {
				lat.addEdgeLocked(edge.SourceID, edge.Relation, funcID)
			}

			currentScopeID = funcID
			continue
		}

		// 7. Class Methods (if inside class body)
		if currentClassID != "" {
			if m := methodRe.FindStringSubmatch(trimmed); len(m) > 1 {
				mName := m[1]
				if mName != "constructor" && !reservedKeywords[mName] {
					methodID := fmt.Sprintf("func_%s_%s_%s", filePath, currentClassID, mName)
					lat.nodes3D[methodID] = &LatticeNode{
						ID:        methodID,
						Dimension: "3D_VOLUMETRIC",
						Type:      "method",
						Name:      mName,
						FilePath:  filePath,
						Line:      lineNum,
						Timestamp: time.Now().Unix(),
					}
					lat.addEdgeLocked(currentClassID, "defines", methodID)
					currentScopeID = methodID
					continue
				}
			}
		}

		// 8. Express / API Routes
		if m := routeRe.FindStringSubmatch(trimmed); len(m) > 2 {
			method := strings.ToUpper(m[1])
			endpoint := m[2]
			routeID := fmt.Sprintf("route_%s_%s_%s", filePath, method, endpoint)
			lat.nodes3D[routeID] = &LatticeNode{
				ID:        routeID,
				Dimension: "3D_VOLUMETRIC",
				Type:      "route",
				Name:      fmt.Sprintf("%s %s", method, endpoint),
				FilePath:  filePath,
				Line:      lineNum,
				Timestamp: time.Now().Unix(),
			}
			lat.addEdgeLocked(fileID, "routes", routeID)
			if currentScopeID != "" {
				lat.addEdgeLocked(currentScopeID, "routes", routeID)
			}
		}

		// 9. JSX Component References (<Button ... />, <UserCard ... />)
		if jsxMatches := jsxCompRe.FindAllStringSubmatch(trimmed, -1); len(jsxMatches) > 0 {
			for _, jm := range jsxMatches {
				compName := jm[1]
				calleeCompID := fmt.Sprintf("func_%s", compName)
				if currentScopeID != "" {
					lat.addEdgeLocked(currentScopeID, "references", calleeCompID)
					lat.addEdgeLocked(calleeCompID, "used_by", currentScopeID)
				}
				lat.addEdgeLocked(fileID, "references", calleeCompID)
				lat.addEdgeLocked(calleeCompID, "used_by", fileID)

				// Also link directly to any already-defined component nodes with this name
				for targetID, node := range lat.nodes3D {
					if node.Name == compName && (node.Type == "component" || node.Type == "function") {
						if currentScopeID != "" {
							lat.addEdgeLocked(currentScopeID, "references", targetID)
							lat.addEdgeLocked(targetID, "used_by", currentScopeID)
						}
						lat.addEdgeLocked(fileID, "references", targetID)
						lat.addEdgeLocked(targetID, "used_by", fileID)
					}
				}
			}
		}

		// 10. Call expressions inside function/method bodies
		if currentScopeID != "" {
			if callMatches := callRe.FindAllStringSubmatch(trimmed, -1); len(callMatches) > 0 {
				for _, cm := range callMatches {
					callee := cm[1]
					if !reservedKeywords[callee] && callee != fnName {
						calleeID := fmt.Sprintf("func_%s", callee)
						lat.addEdgeLocked(currentScopeID, "calls", calleeID)
						lat.addEdgeLocked(calleeID, "called_by", currentScopeID)
					}
				}
			}
		}
	}

	return nil
}

func (lat *LocalMemoryCache) ingestHTMLAST(filePath string, src []byte) error {
	lat.mu.Lock()
	defer lat.mu.Unlock()

	fileID := fmt.Sprintf("file_%s", filePath)
	lat.nodes3D[fileID] = &LatticeNode{
		ID:        fileID,
		Dimension: "3D_VOLUMETRIC",
		Type:      "file",
		Name:      filePath,
		FilePath:  filePath,
		Timestamp: time.Now().Unix(),
	}

	elemRe := regexp.MustCompile(`<([a-zA-Z0-9]+)\s+[^>]*(?:id=['"]([^'"]+)['"]|onclick=['"]([^'"]+)['"])[^>]*>`)
	lines := strings.Split(string(src), "\n")
	for lineIdx, line := range lines {
		lineNum := lineIdx + 1
		matches := elemRe.FindAllStringSubmatch(line, -1)
		for _, m := range matches {
			tag := m[1]
			id := m[2]
			onclick := m[3]
			if id != "" {
				nodeID := fmt.Sprintf("elem_%s_%s", filePath, id)
				lat.nodes3D[nodeID] = &LatticeNode{
					ID:        nodeID,
					Dimension: "3D_VOLUMETRIC",
					Type:      "element",
					Name:      fmt.Sprintf("<%s id=%q>", tag, id),
					FilePath:  filePath,
					Line:      lineNum,
					Timestamp: time.Now().Unix(),
				}
				lat.addEdgeLocked(fileID, "defines", nodeID)
			}
			if onclick != "" {
				fnName := strings.Split(onclick, "(")[0]
				fnName = strings.TrimSpace(fnName)
				if fnName != "" {
					fnID := fmt.Sprintf("func_%s", fnName)
					lat.addEdgeLocked(fileID, "calls", fnID)
				}
			}
		}
	}

	scriptRe := regexp.MustCompile(`(?s)<script[^>]*>(.*?)</script>`)
	scriptMatches := scriptRe.FindAllSubmatch(src, -1)
	for _, sm := range scriptMatches {
		if len(sm) > 1 {
			funcRe := regexp.MustCompile(`(?:function\s+([a-zA-Z0-9_]+)|(?:const|let|var)\s+([a-zA-Z0-9_]+)\s*=\s*(?:async\s+)?(?:\([^)]*\)|[a-zA-Z0-9_]+)\s*=>)`)
			for _, fm := range funcRe.FindAllStringSubmatch(string(sm[1]), -1) {
				name := fm[1]
				if name == "" && len(fm) > 2 {
					name = fm[2]
				}
				if name != "" {
					funcID := fmt.Sprintf("func_%s_%s", filePath, name)
					lat.nodes3D[funcID] = &LatticeNode{
						ID:        funcID,
						Dimension: "3D_VOLUMETRIC",
						Type:      "function",
						Name:      name,
						FilePath:  filePath,
						Timestamp: time.Now().Unix(),
					}
					lat.addEdgeLocked(fileID, "defines", funcID)
				}
			}
		}
	}

	return nil
}

func (lat *LocalMemoryCache) ingestRustAST(filePath string, src []byte) error {
	lat.mu.Lock()
	defer lat.mu.Unlock()

	fileID := fmt.Sprintf("file_%s", filePath)
	lat.nodes3D[fileID] = &LatticeNode{
		ID:        fileID,
		Dimension: "3D_VOLUMETRIC",
		Type:      "file",
		Name:      filePath,
		FilePath:  filePath,
		Timestamp: time.Now().Unix(),
	}

	lines := strings.Split(string(src), "\n")
	useRe := regexp.MustCompile(`^\s*(?:pub\s+)?use\s+([a-zA-Z0-9_:]+)`)
	structRe := regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?(?:struct|enum|trait|type)\s+([a-zA-Z0-9_]+)`)
	fnRe := regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?(?:async\s+)?(?:const\s+)?fn\s+([a-zA-Z0-9_]+)`)
	implRe := regexp.MustCompile(`^\s*impl(?:\s*<[^>]*>)?\s+(?:([a-zA-Z0-9_]+)\s+for\s+)?([a-zA-Z0-9_]+)`)

	for lineIdx, line := range lines {
		lineNum := lineIdx + 1
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}

		if m := useRe.FindStringSubmatch(trimmed); len(m) > 1 {
			pkgID := fmt.Sprintf("pkg_%s", m[1])
			lat.addEdgeLocked(fileID, "imports", pkgID)
			continue
		}

		if m := structRe.FindStringSubmatch(trimmed); len(m) > 1 {
			typeName := m[1]
			typeID := fmt.Sprintf("type_%s_%s", filePath, typeName)
			lat.nodes3D[typeID] = &LatticeNode{
				ID:        typeID,
				Dimension: "3D_VOLUMETRIC",
				Type:      "struct",
				Name:      typeName,
				FilePath:  filePath,
				Line:      lineNum,
				Timestamp: time.Now().Unix(),
			}
			lat.addEdgeLocked(fileID, "defines", typeID)
			continue
		}

		if m := implRe.FindStringSubmatch(trimmed); len(m) > 2 {
			targetName := m[2]
			typeID := fmt.Sprintf("type_%s_%s", filePath, targetName)
			lat.addEdgeLocked(fileID, "implements", typeID)
		}

		if m := fnRe.FindStringSubmatch(trimmed); len(m) > 1 {
			fnName := m[1]
			fnID := fmt.Sprintf("func_%s_%s", filePath, fnName)
			lat.nodes3D[fnID] = &LatticeNode{
				ID:        fnID,
				Dimension: "3D_VOLUMETRIC",
				Type:      "function",
				Name:      fnName,
				FilePath:  filePath,
				Line:      lineNum,
				Timestamp: time.Now().Unix(),
			}
			lat.addEdgeLocked(fileID, "defines", fnID)
			continue
		}
	}

	return nil
}

func (lat *LocalMemoryCache) ingestCppAST(filePath string, src []byte) error {
	lat.mu.Lock()
	defer lat.mu.Unlock()

	fileID := fmt.Sprintf("file_%s", filePath)
	lat.nodes3D[fileID] = &LatticeNode{
		ID:        fileID,
		Dimension: "3D_VOLUMETRIC",
		Type:      "file",
		Name:      filePath,
		FilePath:  filePath,
		Timestamp: time.Now().Unix(),
	}

	lines := strings.Split(string(src), "\n")
	includeRe := regexp.MustCompile(`^\s*#include\s+[<"]([^>"]+)[>"]`)
	classRe := regexp.MustCompile(`^\s*(?:class|struct)\s+([a-zA-Z0-9_]+)`)
	funcRe := regexp.MustCompile(`^\s*(?:[a-zA-Z0-9_:<>&*~]+\s+)+([a-zA-Z0-9_]+)\s*\([^)]*\)\s*(?:const)?\s*(?:override)?\s*[{;]`)

	for lineIdx, line := range lines {
		lineNum := lineIdx + 1
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}

		if m := includeRe.FindStringSubmatch(trimmed); len(m) > 1 {
			pkgID := fmt.Sprintf("pkg_%s", m[1])
			lat.addEdgeLocked(fileID, "imports", pkgID)
			continue
		}

		if m := classRe.FindStringSubmatch(trimmed); len(m) > 1 {
			className := m[1]
			classID := fmt.Sprintf("type_%s_%s", filePath, className)
			lat.nodes3D[classID] = &LatticeNode{
				ID:        classID,
				Dimension: "3D_VOLUMETRIC",
				Type:      "class",
				Name:      className,
				FilePath:  filePath,
				Line:      lineNum,
				Timestamp: time.Now().Unix(),
			}
			lat.addEdgeLocked(fileID, "defines", classID)
			continue
		}

		if m := funcRe.FindStringSubmatch(trimmed); len(m) > 1 {
			fnName := m[1]
			if fnName != "if" && fnName != "for" && fnName != "while" && fnName != "switch" {
				fnID := fmt.Sprintf("func_%s_%s", filePath, fnName)
				lat.nodes3D[fnID] = &LatticeNode{
					ID:        fnID,
					Dimension: "3D_VOLUMETRIC",
					Type:      "function",
					Name:      fnName,
					FilePath:  filePath,
					Line:      lineNum,
					Timestamp: time.Now().Unix(),
				}
				lat.addEdgeLocked(fileID, "defines", fnID)
				continue
			}
		}
	}

	return nil
}

func (lat *LocalMemoryCache) ingestJavaAST(filePath string, src []byte) error {
	lat.mu.Lock()
	defer lat.mu.Unlock()

	fileID := fmt.Sprintf("file_%s", filePath)
	lat.nodes3D[fileID] = &LatticeNode{
		ID:        fileID,
		Dimension: "3D_VOLUMETRIC",
		Type:      "file",
		Name:      filePath,
		FilePath:  filePath,
		Timestamp: time.Now().Unix(),
	}

	lines := strings.Split(string(src), "\n")
	importRe := regexp.MustCompile(`^\s*import\s+(?:static\s+)?([a-zA-Z0-9_.*]+);`)
	classRe := regexp.MustCompile(`^\s*(?:public|protected|private)?\s*(?:abstract|static|final)?\s*(?:class|interface|enum|record)\s+([a-zA-Z0-9_]+)`)
	methodRe := regexp.MustCompile(`^\s*(?:public|protected|private)?\s*(?:static\s+)?(?:final\s+)?(?:[a-zA-Z0-9_<>[\]]+\s+)+([a-zA-Z0-9_]+)\s*\([^)]*\)\s*(?:throws\s+[a-zA-Z0-9_,\s]+)?\s*[{;]`)

	for lineIdx, line := range lines {
		lineNum := lineIdx + 1
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}

		if m := importRe.FindStringSubmatch(trimmed); len(m) > 1 {
			pkgID := fmt.Sprintf("pkg_%s", m[1])
			lat.addEdgeLocked(fileID, "imports", pkgID)
			continue
		}

		if m := classRe.FindStringSubmatch(trimmed); len(m) > 1 {
			className := m[1]
			classID := fmt.Sprintf("type_%s_%s", filePath, className)
			lat.nodes3D[classID] = &LatticeNode{
				ID:        classID,
				Dimension: "3D_VOLUMETRIC",
				Type:      "class",
				Name:      className,
				FilePath:  filePath,
				Line:      lineNum,
				Timestamp: time.Now().Unix(),
			}
			lat.addEdgeLocked(fileID, "defines", classID)
			continue
		}

		if m := methodRe.FindStringSubmatch(trimmed); len(m) > 1 {
			methodName := m[1]
			if methodName != "if" && methodName != "for" && methodName != "while" && methodName != "switch" {
				fnID := fmt.Sprintf("func_%s_%s", filePath, methodName)
				lat.nodes3D[fnID] = &LatticeNode{
					ID:        fnID,
					Dimension: "3D_VOLUMETRIC",
					Type:      "method",
					Name:      methodName,
					FilePath:  filePath,
					Line:      lineNum,
					Timestamp: time.Now().Unix(),
				}
				lat.addEdgeLocked(fileID, "defines", fnID)
				continue
			}
		}
	}

	return nil
}

func (lat *LocalMemoryCache) ingestSqlAST(filePath string, src []byte) error {
	lat.mu.Lock()
	defer lat.mu.Unlock()

	fileID := fmt.Sprintf("file_%s", filePath)
	lat.nodes3D[fileID] = &LatticeNode{
		ID:        fileID,
		Dimension: "3D_VOLUMETRIC",
		Type:      "file",
		Name:      filePath,
		FilePath:  filePath,
		Timestamp: time.Now().Unix(),
	}

	tableRe := regexp.MustCompile(`(?i)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-zA-Z0-9_"]+)`)
	indexRe := regexp.MustCompile(`(?i)CREATE\s+(?:UNIQUE\s+)?INDEX\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-zA-Z0-9_"]+)\s+ON\s+([a-zA-Z0-9_"]+)`)
	viewRe := regexp.MustCompile(`(?i)CREATE\s+VIEW\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-zA-Z0-9_"]+)`)

	content := string(src)
	for _, m := range tableRe.FindAllStringSubmatch(content, -1) {
		if len(m) > 1 {
			tableName := strings.Trim(m[1], `"'`)
			tblID := fmt.Sprintf("table_%s_%s", filePath, tableName)
			lat.nodes3D[tblID] = &LatticeNode{
				ID:        tblID,
				Dimension: "3D_VOLUMETRIC",
				Type:      "table",
				Name:      tableName,
				FilePath:  filePath,
				Timestamp: time.Now().Unix(),
			}
			lat.addEdgeLocked(fileID, "defines", tblID)
		}
	}

	for _, m := range indexRe.FindAllStringSubmatch(content, -1) {
		if len(m) > 2 {
			idxName := strings.Trim(m[1], `"'`)
			tblName := strings.Trim(m[2], `"'`)
			idxID := fmt.Sprintf("index_%s_%s", filePath, idxName)
			tblID := fmt.Sprintf("table_%s_%s", filePath, tblName)
			lat.nodes3D[idxID] = &LatticeNode{
				ID:        idxID,
				Dimension: "3D_VOLUMETRIC",
				Type:      "index",
				Name:      idxName,
				FilePath:  filePath,
				Timestamp: time.Now().Unix(),
			}
			lat.addEdgeLocked(fileID, "defines", idxID)
			lat.addEdgeLocked(idxID, "indexes", tblID)
		}
	}

	for _, m := range viewRe.FindAllStringSubmatch(content, -1) {
		if len(m) > 1 {
			viewName := strings.Trim(m[1], `"'`)
			viewID := fmt.Sprintf("view_%s_%s", filePath, viewName)
			lat.nodes3D[viewID] = &LatticeNode{
				ID:        viewID,
				Dimension: "3D_VOLUMETRIC",
				Type:      "view",
				Name:      viewName,
				FilePath:  filePath,
				Timestamp: time.Now().Unix(),
			}
			lat.addEdgeLocked(fileID, "defines", viewID)
		}
	}

	return nil
}


