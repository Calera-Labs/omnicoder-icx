// SPDX-License-Identifier: Apache-2.0
// Package orchestrator implements the isolated multi-process parallel agent swarm engine
// for OmniCoder-ICX.
//
// ICX 4D Volumetric Swarm Variable Store & Wave 0 Contract Freezing.
// Crystallizes shared interface contracts in Wave 0, publishes them to the ICX Swarm Store,
// dynamically injects them into sibling workers' prompt cards, and facilitates dynamic variable exchange.

package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/canonical"
	"github.com/caleralabs/omnicoder-icx/pkg/icx"
)

var (
	// ErrNoContractsFound is returned when no contracts are discovered for a requested package.
	ErrNoContractsFound = errors.New("no frozen contracts found for package")
)

// ExportedContract captures the signature of an exported struct, interface, or function.
type ExportedContract struct {
	PackageName string `json:"package_name"`
	SymbolName  string `json:"symbol_name"`
	Kind        string `json:"kind"` // "struct", "interface", "func", "const"
	Signature   string `json:"signature"`
	MerkleHash  string `json:"merkle_hash"`
}

// CanonicalASTRibbon produces a canonical 1D AST ribbon for an exported contract.
func CanonicalASTRibbon(c ExportedContract) string {
	return fmt.Sprintf("<ast:contract pkg=%q symbol=%q kind=%q signature=%q/>", c.PackageName, c.SymbolName, c.Kind, c.Signature)
}

// CanonicalASTHash computes the deterministic SHA-256 Merkle hash for a contract.
func CanonicalASTHash(c ExportedContract) string {
	return canonical.SHA256Bytes([]byte(CanonicalASTRibbon(c)))
}

// ContractRegistry manages frozen contracts across swarm waves.
type ContractRegistry struct {
	store          *icx.CASStore
	spaceID        string
	phaseContracts map[int][]string // phaseID -> slice of contract keys
	mu             sync.RWMutex
}

// NewContractRegistry initializes a new ContractRegistry with the specified CASStore and spaceID.
func NewContractRegistry(store *icx.CASStore, spaceID string) *ContractRegistry {
	if store == nil {
		store = icx.NewCASStore()
	}
	if spaceID == "" {
		spaceID = "default"
	}
	return &ContractRegistry{
		store:          store,
		spaceID:        spaceID,
		phaseContracts: make(map[int][]string),
	}
}

// Store returns the underlying ICX CASStore.
func (cr *ContractRegistry) Store() *icx.CASStore {
	return cr.store
}

// SpaceID returns the target memory space identifier.
func (cr *ContractRegistry) SpaceID() string {
	return cr.spaceID
}

// ExtractAndPublish parses all Go files written in Wave 0 using Go's native go/parser and go/ast.
// All exported identifiers (capitalized structs, interfaces, type aliases, function signatures)
// are serialized into canonical AST ribbons and committed to icx.CASStore under keys contract/<package>/<symbol>.
func (cr *ContractRegistry) ExtractAndPublish(ctx context.Context, filePath string, content []byte) ([]ExportedContract, error) {
	if len(content) == 0 && filePath != "" {
		diskBytes, err := os.ReadFile(filePath)
		if err == nil {
			content = diskBytes
		}
	}

	if len(content) == 0 {
		return nil, fmt.Errorf("cannot extract contracts: content is empty for %s", filePath)
	}

	fset := token.NewFileSet()
	fileNode, err := parser.ParseFile(fset, filePath, content, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("failed to parse Go file %s: %w", filePath, err)
	}

	pkgName := fileNode.Name.Name
	var contracts []ExportedContract

	for _, decl := range fileNode.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			switch d.Tok {
			case token.TYPE:
				for _, spec := range d.Specs {
					typeSpec, ok := spec.(*ast.TypeSpec)
					if !ok || !ast.IsExported(typeSpec.Name.Name) {
						continue
					}

					kind := "type"
					sig := fmt.Sprintf("type %s", typeSpec.Name.Name)

					switch typeSpec.Type.(type) {
					case *ast.StructType:
						kind = "struct"
						sig = fmt.Sprintf("type %s struct", typeSpec.Name.Name)
					case *ast.InterfaceType:
						kind = "interface"
						sig = fmt.Sprintf("type %s interface", typeSpec.Name.Name)
					default:
						var buf bytes.Buffer
						if err := format.Node(&buf, fset, typeSpec.Type); err == nil {
							sig = fmt.Sprintf("type %s %s", typeSpec.Name.Name, buf.String())
						}
					}

					c := ExportedContract{
						PackageName: pkgName,
						SymbolName:  typeSpec.Name.Name,
						Kind:        kind,
						Signature:   sig,
					}
					c.MerkleHash = CanonicalASTHash(c)
					contracts = append(contracts, c)
				}

			case token.CONST:
				for _, spec := range d.Specs {
					valSpec, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, name := range valSpec.Names {
						if !ast.IsExported(name.Name) {
							continue
						}
						sig := fmt.Sprintf("const %s", name.Name)
						if i < len(valSpec.Values) {
							var buf bytes.Buffer
							if err := format.Node(&buf, fset, valSpec.Values[i]); err == nil {
								sig = fmt.Sprintf("const %s = %s", name.Name, buf.String())
							}
						}
						c := ExportedContract{
							PackageName: pkgName,
							SymbolName:  name.Name,
							Kind:        "const",
							Signature:   sig,
						}
						c.MerkleHash = CanonicalASTHash(c)
						contracts = append(contracts, c)
					}
				}
			}

		case *ast.FuncDecl:
			if !ast.IsExported(d.Name.Name) {
				continue
			}

			sig := formatFuncSignature(fset, d)
			c := ExportedContract{
				PackageName: pkgName,
				SymbolName:  d.Name.Name,
				Kind:        "func",
				Signature:   sig,
			}
			c.MerkleHash = CanonicalASTHash(c)
			contracts = append(contracts, c)
		}
	}

	// Commit extracted contracts to icx.CASStore under keys contract/<package>/<symbol>
	cr.mu.Lock()
	defer cr.mu.Unlock()

	for _, c := range contracts {
		key := fmt.Sprintf("contract/%s/%s", c.PackageName, c.SymbolName)
		_, err := cr.store.VarSet(ctx, icx.VarSetRequest{
			SpaceID: cr.spaceID,
			Key:     key,
			Value:   c,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to commit contract %s to CAS store: %w", key, err)
		}
	}

	return contracts, nil
}

// QueryPackageContracts queries all attested contracts belonging to pkgName from icx.CASStore.
func (cr *ContractRegistry) QueryPackageContracts(ctx context.Context, pkgName string) ([]ExportedContract, error) {
	prefix := fmt.Sprintf("contract/%s/", pkgName)
	listResp, err := cr.store.VarList(ctx, icx.VarListRequest{
		SpaceID: cr.spaceID,
		Prefix:  prefix,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list contracts for package %s: %w", pkgName, err)
	}
	if listResp == nil || len(listResp.Variables) == 0 {
		return nil, nil
	}

	var contracts []ExportedContract
	for _, vInfo := range listResp.Variables {
		getResp, err := cr.store.VarGet(ctx, icx.VarGetRequest{
			SpaceID: cr.spaceID,
			Key:     vInfo.Key,
		})
		if err != nil || getResp == nil || !getResp.OK {
			continue
		}

		c, err := parseContractFromValue(getResp.Value)
		if err != nil {
			continue
		}
		contracts = append(contracts, *c)
	}

	// Deterministic ascending sort by SymbolName
	sort.Slice(contracts, func(i, j int) bool {
		return contracts[i].SymbolName < contracts[j].SymbolName
	})

	return contracts, nil
}

// QueryAllContracts retrieves all attested contracts across all frozen packages.
func (cr *ContractRegistry) QueryAllContracts(ctx context.Context) ([]ExportedContract, error) {
	listResp, err := cr.store.VarList(ctx, icx.VarListRequest{
		SpaceID: cr.spaceID,
		Prefix:  "contract/",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list contracts: %w", err)
	}
	if listResp == nil || len(listResp.Variables) == 0 {
		return nil, nil
	}

	var contracts []ExportedContract
	for _, vInfo := range listResp.Variables {
		getResp, err := cr.store.VarGet(ctx, icx.VarGetRequest{
			SpaceID: cr.spaceID,
			Key:     vInfo.Key,
		})
		if err != nil || getResp == nil || !getResp.OK {
			continue
		}

		c, err := parseContractFromValue(getResp.Value)
		if err != nil {
			continue
		}
		contracts = append(contracts, *c)
	}

	sort.Slice(contracts, func(i, j int) bool {
		if contracts[i].PackageName != contracts[j].PackageName {
			return contracts[i].PackageName < contracts[j].PackageName
		}
		return contracts[i].SymbolName < contracts[j].SymbolName
	})

	return contracts, nil
}

// BuildGroundingBlock retrieves all attested contracts and generates a strictly formatted markdown prompt injection.
func (cr *ContractRegistry) BuildGroundingBlock(ctx context.Context, activePhase int, completedPhases []int) (string, error) {
	allContracts, err := cr.QueryAllContracts(ctx)
	if err != nil {
		return "", err
	}

	// Group contracts by package name
	pkgMap := make(map[string][]ExportedContract)
	for _, c := range allContracts {
		pkgMap[c.PackageName] = append(pkgMap[c.PackageName], c)
	}

	var pkgs []string
	for p := range pkgMap {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)

	var sb strings.Builder
	sb.WriteString("=== SWARM COGNITIVE GROUNDING: FROZEN CONTRACTS ===\n")
	sb.WriteString("The following types are FROZEN in Wave 0 and MUST be imported, NOT redeclared:\n")

	for _, pkg := range pkgs {
		contracts := pkgMap[pkg]
		sort.Slice(contracts, func(i, j int) bool {
			return contracts[i].SymbolName < contracts[j].SymbolName
		})

		var typeTokens []string
		for _, c := range contracts {
			if c.Kind == "struct" || c.Kind == "interface" || c.Kind == "type" {
				typeTokens = append(typeTokens, fmt.Sprintf("type %s %s", c.SymbolName, c.Kind))
			}
		}

		// Fallback to functions/consts if package defines no explicit types
		if len(typeTokens) == 0 {
			for _, c := range contracts {
				if c.Kind == "func" {
					typeTokens = append(typeTokens, fmt.Sprintf("func %s", c.SymbolName))
				} else if c.Kind == "const" {
					typeTokens = append(typeTokens, fmt.Sprintf("const %s", c.SymbolName))
				}
			}
		}

		if len(typeTokens) > 0 {
			sb.WriteString(fmt.Sprintf("- package %s: %s\n", pkg, strings.Join(typeTokens, ", ")))
		}
	}

	sb.WriteString("\nCRITICAL INVARIANT: Sibling workers rely on these exact types. \n")
	sb.WriteString("Do NOT duplicate or alter these struct definitions in your scope!\n")

	return sb.String(), nil
}

// RegisterEndpoint publishes a dynamically allocated endpoint with a default 1-hour TTL.
func (cr *ContractRegistry) RegisterEndpoint(ctx context.Context, key, endpoint string, ttl ...time.Duration) error {
	ttlMs := 3600 * 1000 // 1 hour TTL
	if len(ttl) > 0 && ttl[0] > 0 {
		ttlMs = int(ttl[0].Milliseconds())
	}
	_, err := cr.store.VarSet(ctx, icx.VarSetRequest{
		SpaceID: cr.spaceID,
		Key:     key,
		Value:   endpoint,
		TTLMs:   ttlMs,
	})
	return err
}

// DiscoverEndpoint discovers a dynamic endpoint published by a sibling worker.
func (cr *ContractRegistry) DiscoverEndpoint(ctx context.Context, key string) (string, error) {
	resp, err := cr.store.VarGet(ctx, icx.VarGetRequest{
		SpaceID: cr.spaceID,
		Key:     key,
	})
	if err != nil {
		return "", err
	}
	if resp == nil || !resp.OK {
		return "", icx.ErrEndpointNotFound
	}
	if s, ok := resp.Value.(string); ok {
		return s, nil
	}
	return fmt.Sprintf("%v", resp.Value), nil
}

// ComputeContractsMerkleRoot calculates the balanced binary Merkle tree root for a slice of contracts.
func ComputeContractsMerkleRoot(contracts []ExportedContract) string {
	hashes := make([]string, len(contracts))
	for i, c := range contracts {
		hashes[i] = c.MerkleHash
	}
	return canonical.MerkleRoot(hashes)
}

// formatFuncSignature formats an ast.FuncDecl signature excluding its body.
func formatFuncSignature(fset *token.FileSet, fn *ast.FuncDecl) string {
	clone := *fn
	clone.Body = nil
	clone.Doc = nil
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, &clone); err != nil {
		if fn.Recv != nil && len(fn.Recv.List) > 0 {
			return fmt.Sprintf("func (...) %s(...)", fn.Name.Name)
		}
		return fmt.Sprintf("func %s(...)", fn.Name.Name)
	}
	return strings.TrimSpace(buf.String())
}

// parseContractFromValue unmarshals or type-asserts any value into an ExportedContract.
func parseContractFromValue(val any) (*ExportedContract, error) {
	if c, ok := val.(ExportedContract); ok {
		return &c, nil
	}
	if c, ok := val.(*ExportedContract); ok {
		return c, nil
	}
	b, err := json.Marshal(val)
	if err != nil {
		return nil, err
	}
	var c ExportedContract
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}
