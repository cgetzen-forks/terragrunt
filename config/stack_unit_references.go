package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gruntwork-io/terragrunt/internal/errors"
	"github.com/gruntwork-io/terragrunt/pkg/log"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// createStackEvalContext creates an evaluation context for parsing stack files.
// It extends the standard Terragrunt eval context with a placeholder "unit" variable
// that allows unit references to be parsed without errors.
func createStackEvalContext(ctx *ParsingContext, l log.Logger, configPath string, file *hcl.File) (*hcl.EvalContext, error) {
	// First, create the standard Terragrunt eval context
	evalCtx, err := createTerragruntEvalContext(ctx, l, configPath)
	if err != nil {
		return nil, err
	}

	// Add a "unit" variable with a dynamic placeholder
	// This allows the HCL parser to evaluate expressions like unit.dep.result
	// without throwing "unknown variable" errors, even for non-existent units
	// We'll validate that the referenced units actually exist later
	evalCtx.Variables["unit"] = cty.DynamicVal

	return evalCtx, nil
}

// UnitReference represents a reference to another unit's output in a values expression.
type UnitReference struct {
	UnitName   string // Name of the referenced unit
	OutputName string // Name of the output being referenced
}

// UnitDependencies holds information about unit dependencies extracted from values expressions.
type UnitDependencies struct {
	Dependencies map[string][]string // Map of unit name -> list of referenced outputs
	// ValueKeyToUnitRef maps value keys to their unit references
	// e.g., "b" -> {UnitName: "a", OutputName: "a"}
	ValueKeyToUnitRef map[string]UnitReference
}

// NewUnitDependencies creates a new UnitDependencies instance.
func NewUnitDependencies() *UnitDependencies {
	return &UnitDependencies{
		Dependencies:      make(map[string][]string),
		ValueKeyToUnitRef: make(map[string]UnitReference),
	}
}

// AddDependency adds a dependency on a unit's output.
func (ud *UnitDependencies) AddDependency(unitName, outputName string) {
	if _, exists := ud.Dependencies[unitName]; !exists {
		ud.Dependencies[unitName] = []string{}
	}
	// Avoid duplicates
	for _, existing := range ud.Dependencies[unitName] {
		if existing == outputName {
			return
		}
	}
	ud.Dependencies[unitName] = append(ud.Dependencies[unitName], outputName)
}

// AddValueKeyMapping adds a mapping from a value key to a unit reference.
func (ud *UnitDependencies) AddValueKeyMapping(valueKey, unitName, outputName string) {
	ud.ValueKeyToUnitRef[valueKey] = UnitReference{
		UnitName:   unitName,
		OutputName: outputName,
	}
	// Also add to dependencies
	ud.AddDependency(unitName, outputName)
}

// GetDependencyUnitNames returns a list of all unit names that are dependencies.
func (ud *UnitDependencies) GetDependencyUnitNames() []string {
	names := make([]string, 0, len(ud.Dependencies))
	for name := range ud.Dependencies {
		names = append(names, name)
	}
	return names
}

// scanExpressionForUnitReferences scans an HCL expression for unit.<name>.<output> references.
func scanExpressionForUnitReferences(expr hcl.Expression) *UnitDependencies {
	deps := NewUnitDependencies()

	// Get all variable references in the expression
	vars := expr.Variables()

	for _, traversal := range vars {
		// Check if this is a unit reference: unit.<name>.<output>
		if len(traversal) >= 3 {
			if root, ok := traversal[0].(hcl.TraverseRoot); ok && root.Name == "unit" {
				if attr1, ok := traversal[1].(hcl.TraverseAttr); ok {
					unitName := attr1.Name
					if attr2, ok := traversal[2].(hcl.TraverseAttr); ok {
						outputName := attr2.Name
						deps.AddDependency(unitName, outputName)
					}
				}
			}
		}
	}

	return deps
}

// scanValuesForUnitReferences scans a values cty.Value (which should be an object/map)
// for unit references. This requires access to the raw HCL expression.
func scanValuesForUnitReferences(valuesExpr hcl.Expression) *UnitDependencies {
	deps := NewUnitDependencies()

	// If the expression is an object constructor, scan each attribute
	if objExpr, ok := valuesExpr.(*hclsyntax.ObjectConsExpr); ok {
		for _, item := range objExpr.Items {
			// Get the key name
			var valueKey string
			if keyExpr, ok := item.KeyExpr.(*hclsyntax.ObjectConsKeyExpr); ok {
				if keyExpr.Wrapped != nil {
					if scopeExpr, ok := keyExpr.Wrapped.(*hclsyntax.ScopeTraversalExpr); ok {
						if len(scopeExpr.Traversal) > 0 {
							if root, ok := scopeExpr.Traversal[0].(hcl.TraverseRoot); ok {
								valueKey = root.Name
							}
						}
					}
				}
			}

			// Scan the value expression for unit references
			itemDeps := scanExpressionForUnitReferences(item.ValueExpr)
			for unitName, outputs := range itemDeps.Dependencies {
				for _, output := range outputs {
					// If we have a value key and this is a simple unit reference,
					// create a mapping
					if valueKey != "" && len(itemDeps.Dependencies) == 1 && len(outputs) == 1 {
						deps.AddValueKeyMapping(valueKey, unitName, output)
					} else {
						deps.AddDependency(unitName, output)
					}
				}
			}
		}
	}

	return deps
}

// rewriteExpressionBytes rewrites unit.<name>.<output> to dependency.<name>.outputs.<output>
// in the source bytes of an expression. This uses a token-based approach to preserve
// the structure of the expression while rewriting unit references.
func rewriteExpressionBytes(sourceBytes []byte) []byte {
	// Parse the expression as HCL tokens
	tokens, diags := hclsyntax.LexExpression(sourceBytes, "", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		// If we can't parse, return original bytes
		return sourceBytes
	}

	var result []byte
	i := 0

	for i < len(tokens) {
		token := tokens[i]

		// Check if this is the start of a unit reference: unit.name.output
		if token.Type == hclsyntax.TokenIdent && string(token.Bytes) == "unit" {
			// Check if followed by dot, identifier, dot, identifier
			if i+4 < len(tokens) &&
				tokens[i+1].Type == hclsyntax.TokenDot &&
				tokens[i+2].Type == hclsyntax.TokenIdent &&
				tokens[i+3].Type == hclsyntax.TokenDot &&
				tokens[i+4].Type == hclsyntax.TokenIdent {

				// Rewrite: unit.name.output -> dependency.name.outputs.output
				result = append(result, []byte("dependency")...)
				result = append(result, tokens[i+1].Bytes...) // .
				result = append(result, tokens[i+2].Bytes...) // name
				result = append(result, tokens[i+3].Bytes...) // .
				result = append(result, []byte("outputs")...)
				result = append(result, []byte(".")...)
				result = append(result, tokens[i+4].Bytes...) // output

				i += 5
				continue
			}
		}

		// Not a unit reference, keep the token as-is
		result = append(result, token.Bytes...)
		i++
	}

	return result
}

// generateDependencyBlock generates an HCL dependency block for a unit reference.
// If mockOutputs is provided, it will be used; otherwise, auto-generated mock outputs are created.
func generateDependencyBlock(unitName, configPath string, outputs []string, mockOutputs *cty.Value) *hclwrite.Block {
	block := hclwrite.NewBlock("dependency", []string{unitName})
	body := block.Body()
	body.SetAttributeValue("config_path", cty.StringVal(configPath))

	// Add mock_outputs if provided by the unit or if we need to generate them
	if mockOutputs != nil {
		// Use the mock outputs provided by the unit
		body.SetAttributeValue("mock_outputs", *mockOutputs)

		// Also set mock_outputs_allowed_terraform_commands
		body.SetAttributeValue("mock_outputs_allowed_terraform_commands", cty.ListVal([]cty.Value{
			cty.StringVal("validate"),
			cty.StringVal("plan"),
		}))
	}

	return block
}

// extractUnitValuesExpression extracts the raw HCL expression for the values attribute
// from a unit block in the parsed HCL file.
func extractUnitValuesExpression(file *hcl.File, unitName string) (hcl.Expression, error) {
	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return nil, fmt.Errorf("file body is not hclsyntax.Body")
	}

	for _, block := range body.Blocks {
		if block.Type == "unit" && len(block.Labels) > 0 && block.Labels[0] == unitName {
			if attr, exists := block.Body.Attributes["values"]; exists {
				return attr.Expr, nil
			}
		}
	}

	return nil, fmt.Errorf("unit %s not found or has no values attribute", unitName)
}

// extractStackValuesExpression extracts the raw HCL expression for the values attribute
// from a stack block in the parsed HCL file.
func extractStackValuesExpression(file *hcl.File, stackName string) (hcl.Expression, error) {
	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return nil, fmt.Errorf("file body is not hclsyntax.Body")
	}

	for _, block := range body.Blocks {
		if block.Type == "stack" && len(block.Labels) > 0 && block.Labels[0] == stackName {
			if attr, exists := block.Body.Attributes["values"]; exists {
				return attr.Expr, nil
			}
		}
	}

	return nil, fmt.Errorf("stack %s not found or has no values attribute", stackName)
}

// extractAndProcessUnitReferences extracts raw HCL expressions from units and stacks,
// scans them for unit references, and populates the ValuesExpr and UnitDependencies fields.
func extractAndProcessUnitReferences(file *hcl.File, config *StackConfigFile) error {
	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		// If not hclsyntax.Body, we can't extract raw expressions
		// This is fine - just means no unit references to process
		return nil
	}

	// Build a map of all unit names for validation
	unitNames := make(map[string]bool)
	for _, unit := range config.Units {
		unitNames[unit.Name] = true
	}

	// Store source bytes for all units and stacks
	sourceBytes := file.Bytes

	// Process units
	for _, unit := range config.Units {
		unit.SourceBytes = sourceBytes
		for _, block := range body.Blocks {
			if block.Type == "unit" && len(block.Labels) > 0 && block.Labels[0] == unit.Name {
				if attr, exists := block.Body.Attributes["values"]; exists {
					unit.ValuesExpr = attr.Expr
					unit.UnitDependencies = scanValuesForUnitReferences(attr.Expr)

					// Validate that all referenced units exist
					if err := validateUnitReferences(unit.Name, unit.UnitDependencies, unitNames); err != nil {
						return err
					}
				}
				break
			}
		}
	}

	// Process stacks
	for _, stack := range config.Stacks {
		stack.SourceBytes = sourceBytes
		for _, block := range body.Blocks {
			if block.Type == "stack" && len(block.Labels) > 0 && block.Labels[0] == stack.Name {
				if attr, exists := block.Body.Attributes["values"]; exists {
					stack.ValuesExpr = attr.Expr
					stack.UnitDependencies = scanValuesForUnitReferences(attr.Expr)

					// Validate that all referenced units exist
					if err := validateUnitReferences(stack.Name, stack.UnitDependencies, unitNames); err != nil {
						return err
					}
				}
				break
			}
		}
	}

	// Check for cyclic dependencies
	if err := detectCyclicDependencies(config); err != nil {
		return err
	}

	return nil
}

// validateUnitReferences validates that all referenced units exist in the stack.
func validateUnitReferences(sourceName string, deps *UnitDependencies, validUnitNames map[string]bool) error {
	if deps == nil {
		return nil
	}

	for unitName := range deps.Dependencies {
		if !validUnitNames[unitName] {
			return fmt.Errorf(
				"unit or stack '%s' references non-existent unit '%s' in its values.\n"+
					"Available units: %v\n"+
					"Please ensure the referenced unit is defined in the same stack file.",
				sourceName, unitName, getMapKeys(validUnitNames))
		}
	}

	return nil
}

// detectCyclicDependencies checks for cyclic dependencies in the unit dependency graph.
func detectCyclicDependencies(config *StackConfigFile) error {
	// Build dependency graph
	graph := make(map[string][]string)

	for _, unit := range config.Units {
		if unit.UnitDependencies != nil {
			graph[unit.Name] = unit.UnitDependencies.GetDependencyUnitNames()
		}
	}

	for _, stack := range config.Stacks {
		if stack.UnitDependencies != nil {
			graph[stack.Name] = stack.UnitDependencies.GetDependencyUnitNames()
		}
	}

	// Detect cycles using DFS
	visited := make(map[string]bool)
	recStack := make(map[string]bool)

	var detectCycle func(string, []string) error
	detectCycle = func(node string, path []string) error {
		visited[node] = true
		recStack[node] = true
		path = append(path, node)

		for _, dep := range graph[node] {
			if !visited[dep] {
				if err := detectCycle(dep, path); err != nil {
					return err
				}
			} else if recStack[dep] {
				// Found a cycle
				cyclePath := append(path, dep)
				return fmt.Errorf(
					"cyclic dependency detected: %s\n"+
						"Units cannot have circular dependencies. Please restructure your stack to remove the cycle.",
					formatCyclePath(cyclePath))
			}
		}

		recStack[node] = false
		return nil
	}

	for node := range graph {
		if !visited[node] {
			if err := detectCycle(node, []string{}); err != nil {
				return err
			}
		}
	}

	return nil
}

// getMapKeys returns the keys of a map as a slice.
func getMapKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// formatCyclePath formats a cycle path for error messages.
func formatCyclePath(path []string) string {
	if len(path) == 0 {
		return ""
	}
	result := path[0]
	for i := 1; i < len(path); i++ {
		result += " -> " + path[i]
	}
	return result
}

// calculateRelativeConfigPath calculates the relative path from the current unit directory
// to the dependency unit directory for use in dependency blocks.
func calculateRelativeConfigPath(stackTargetDir, currentUnitDir, depUnitName string) string {
	// The dependency unit is at stackTargetDir/depUnitName
	// We need a relative path from currentUnitDir to that location
	depUnitDir := filepath.Join(stackTargetDir, depUnitName)

	// Calculate relative path
	relPath, err := filepath.Rel(currentUnitDir, depUnitDir)
	if err != nil {
		// Fallback to absolute path if relative path calculation fails
		return depUnitDir
	}

	return relPath
}

// injectDependencyBlocks reads the terragrunt.hcl file, injects dependency blocks at the top,
// modifies the inputs block to resolve unit references, and writes it back.
func injectDependencyBlocks(l log.Logger, terragruntHclPath string, unitDeps *UnitDependencies, stackTargetDir, unitDir string) error {
	// Read the existing terragrunt.hcl file
	existingBytes, err := os.ReadFile(terragruntHclPath)
	if err != nil {
		return fmt.Errorf("failed to read %s: %w", terragruntHclPath, err)
	}

	// Parse it to preserve formatting
	file, diags := hclwrite.ParseConfig(existingBytes, terragruntHclPath, hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		return fmt.Errorf("failed to parse %s: %s", terragruntHclPath, diags.Error())
	}

	// Create a new file with dependency blocks at the top
	newFile := hclwrite.NewEmptyFile()
	newBody := newFile.Body()

	// Add dependency blocks
	depNames := unitDeps.GetDependencyUnitNames()
	sort.Strings(depNames) // Sort for deterministic output

	for _, depName := range depNames {
		// Calculate relative path from this unit to the dependency unit
		configPath := calculateRelativeConfigPath(stackTargetDir, unitDir, depName)
		outputs := unitDeps.Dependencies[depName]
		depBlock := generateDependencyBlock(depName, configPath, outputs, nil)
		newBody.AppendBlock(depBlock)
		newBody.AppendNewline()
	}

	// Process the original content to rewrite values.* references in inputs attribute
	originalBody := file.Body()

	// Copy blocks as-is
	for _, block := range originalBody.Blocks() {
		newBody.AppendBlock(block)
	}

	// Process attributes - rewrite inputs attribute if it exists
	for name, attr := range originalBody.Attributes() {
		if name == "inputs" {
			// Rewrite the inputs attribute to resolve unit references
			rewrittenTokens := rewriteInputsAttribute(attr, unitDeps)
			newBody.SetAttributeRaw(name, rewrittenTokens)
		} else {
			// Copy other attributes as-is
			newBody.SetAttributeRaw(name, attr.Expr().BuildTokens(nil))
		}
	}

	// Write back
	if err := os.WriteFile(terragruntHclPath, newFile.Bytes(), 0644); err != nil {
		return fmt.Errorf("failed to write %s: %w", terragruntHclPath, err)
	}

	l.Debugf("Injected %d dependency blocks into %s", len(depNames), terragruntHclPath)
	return nil
}

// rewriteInputsAttribute rewrites an inputs attribute to resolve unit references.
// It transforms values.x to dependency.unit.outputs.y when x comes from a unit dependency.
func rewriteInputsAttribute(attr *hclwrite.Attribute, unitDeps *UnitDependencies) hclwrite.Tokens {
	// Get the original tokens
	tokens := attr.Expr().BuildTokens(nil)

	// Convert to bytes for rewriting
	tokenBytes := tokens.Bytes()

	// Rewrite the expression
	rewrittenBytes := rewriteInputsExpression(tokenBytes, unitDeps)

	// Parse back to tokens
	newTokens, diags := hclsyntax.LexExpression(rewrittenBytes, "", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		// If parsing fails, return original tokens
		return tokens
	}

	var hclTokens hclwrite.Tokens
	for _, token := range newTokens {
		if token.Type == hclsyntax.TokenEOF {
			continue
		}
		hclTokens = append(hclTokens, &hclwrite.Token{
			Type:  token.Type,
			Bytes: token.Bytes,
		})
	}

	return hclTokens
}

// rewriteInputsExpression rewrites an inputs expression to resolve unit references.
func rewriteInputsExpression(exprBytes []byte, unitDeps *UnitDependencies) []byte {
	exprStr := string(exprBytes)

	// For each value key mapping, replace values.key with dependency.unit.outputs.output
	for valueKey, unitRef := range unitDeps.ValueKeyToUnitRef {
		oldPattern := fmt.Sprintf("values.%s", valueKey)
		newPattern := fmt.Sprintf("dependency.%s.outputs.%s", unitRef.UnitName, unitRef.OutputName)
		exprStr = strings.ReplaceAll(exprStr, oldPattern, newPattern)
	}

	return []byte(exprStr)
}

// writeValuesWithDependencyBlocks writes a terragrunt.values.hcl file with dependency blocks
// and a values block containing rewritten expressions.
func writeValuesWithDependencyBlocks(l log.Logger, valuesExpr hcl.Expression, unitDeps *UnitDependencies, unitMockOutputs map[string]*cty.Value, sourceBytes []byte, stackTargetDir, directory string) error {
	file := hclwrite.NewEmptyFile()
	body := file.Body()

	// Add header comment
	body.AppendUnstructuredTokens(hclwrite.Tokens{
		{Type: hclsyntax.TokenComment, Bytes: []byte("# Auto-generated by the terragrunt.stack.hcl file by Terragrunt. Do not edit manually\n")},
	})

	// Add dependency blocks
	depNames := make([]string, 0, len(unitDeps.Dependencies))
	for depName := range unitDeps.Dependencies {
		depNames = append(depNames, depName)
	}
	sort.Strings(depNames) // Sort for deterministic output

	for _, depName := range depNames {
		// Calculate relative path from this unit to the dependency unit
		configPath := calculateRelativeConfigPath(stackTargetDir, directory, depName)
		outputs := unitDeps.Dependencies[depName]

		// Get mock outputs for this dependency unit if available
		var mockOutputs *cty.Value
		if unitMockOutputs != nil {
			mockOutputs = unitMockOutputs[depName]
		}

		depBlock := generateDependencyBlock(depName, configPath, outputs, mockOutputs)
		body.AppendBlock(depBlock)
		body.AppendNewline()
	}

	// Add values block with rewritten expressions
	valuesBlock := hclwrite.NewBlock("values", nil)
	valuesBody := valuesBlock.Body()

	// Write rewritten attributes to the values block
	if err := writeRewrittenValuesAttributes(valuesBody, valuesExpr, unitDeps, sourceBytes); err != nil {
		return err
	}

	body.AppendBlock(valuesBlock)
	body.AppendNewline()

	// Write to file
	valuesPath := filepath.Join(directory, "terragrunt.values.hcl")
	if err := os.WriteFile(valuesPath, file.Bytes(), 0644); err != nil {
		return errors.Errorf("failed to write %s: %w", valuesPath, err)
	}

	l.Infof("Generated %s with dependency blocks", valuesPath)
	return nil
}

// writeRewrittenValuesAttributes writes attributes to the values block, rewriting unit references
// to dependency references in the process.
func writeRewrittenValuesAttributes(body *hclwrite.Body, valuesExpr hcl.Expression, unitDeps *UnitDependencies, sourceBytes []byte) error {
	if valuesExpr == nil {
		return nil
	}

	// Extract the object constructor expression
	objExpr, ok := valuesExpr.(*hclsyntax.ObjectConsExpr)
	if !ok {
		return fmt.Errorf("values expression is not an object constructor")
	}

	// Process each item in the object
	for _, item := range objExpr.Items {
		// Get the key name
		keyExpr, ok := item.KeyExpr.(*hclsyntax.ObjectConsKeyExpr)
		if !ok {
			continue
		}

		// Extract key name from the wrapped expression
		var keyName string
		if keyExpr.Wrapped != nil {
			if scopeExpr, ok := keyExpr.Wrapped.(*hclsyntax.ScopeTraversalExpr); ok {
				if len(scopeExpr.Traversal) > 0 {
					if root, ok := scopeExpr.Traversal[0].(hcl.TraverseRoot); ok {
						keyName = root.Name
					}
				}
			}
		}

		if keyName == "" {
			continue
		}

		// Get the source bytes for the value expression and rewrite it
		valueSourceBytes := item.ValueExpr.Range().SliceBytes(sourceBytes)
		rewrittenBytes := rewriteExpressionBytes(valueSourceBytes)

		// Parse the rewritten bytes as tokens
		tokens, diags := hclsyntax.LexExpression(rewrittenBytes, "", hcl.Pos{Line: 1, Column: 1})
		if diags.HasErrors() {
			// If rewriting failed, use original expression
			body.SetAttributeRaw(keyName, hclwrite.TokensForValue(cty.StringVal(string(valueSourceBytes))))
			continue
		}

		// Convert to hclwrite tokens
		var hclTokens hclwrite.Tokens
		for _, token := range tokens {
			if token.Type == hclsyntax.TokenEOF {
				continue
			}
			hclTokens = append(hclTokens, &hclwrite.Token{
				Type:  token.Type,
				Bytes: token.Bytes,
			})
		}

		body.SetAttributeRaw(keyName, hclTokens)
	}

	return nil
}
