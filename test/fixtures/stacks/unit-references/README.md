# Unit References Test Fixture

This test fixture demonstrates the unit references feature for Terragrunt stacks, which allows units to reference outputs from other units directly in their `values` map.

## Feature Overview

The unit references feature allows you to write:

```hcl
unit "dep" {
  source = "modules/dep"
  path   = "dep"
  values = {
    message = "Hello"
  }
}

unit "app" {
  source = "modules/app"
  path   = "app"
  values = {
    dep_message = unit.dep.result
  }
}
```

And Terragrunt will automatically:
1. Generate a `dependency` block in `app/terragrunt.values.hcl`
2. Rewrite `unit.dep.result` to `dependency.dep.outputs.result`

## Test Cases

### 1. Simple Unit Reference (`terragrunt.stack.hcl`)
- **Description**: Basic test with one unit depending on another
- **Units**: `dep`, `app`
- **Expected**: `app` should have a dependency on `dep` and reference its output

### 2. Complex Multi-Level Dependencies (`terragrunt-complex.stack.hcl`)
- **Description**: Tests multiple dependencies and string interpolation
- **Units**: `base`, `middle`, `top`
- **Dependencies**: 
  - `middle` depends on `base`
  - `top` depends on both `base` and `middle`
- **Expected**: Correct dependency blocks and rewritten expressions with string interpolation

### 3. Non-Existent Unit Reference (`terragrunt-error.stack.hcl`)
- **Description**: Tests error handling for references to non-existent units
- **Expected**: Clear error message indicating which unit doesn't exist

### 4. Cyclic Dependency (`terragrunt-cycle.stack.hcl`)
- **Description**: Tests cyclic dependency detection
- **Units**: `a` depends on `b`, `b` depends on `a`
- **Expected**: Clear error message indicating the cycle

## Running Tests

```bash
# Test simple case
cd live
go run ../../../../../main.go stack generate
cat .terragrunt-stack/app/terragrunt.values.hcl

# Test complex case
mv terragrunt.stack.hcl terragrunt-simple.stack.hcl
mv terragrunt-complex.stack.hcl terragrunt.stack.hcl
go run ../../../../../main.go stack generate
cat .terragrunt-stack/top/terragrunt.values.hcl

# Test error handling
mv terragrunt.stack.hcl terragrunt-complex.stack.hcl
mv terragrunt-error.stack.hcl terragrunt.stack.hcl
go run ../../../../../main.go stack generate  # Should fail with clear error

# Test cyclic dependency detection
mv terragrunt.stack.hcl terragrunt-error.stack.hcl
mv terragrunt-cycle.stack.hcl terragrunt.stack.hcl
go run ../../../../../main.go stack generate  # Should fail with cycle error
```

## Implementation Details

The feature is implemented in:
- `config/stack_unit_references.go`: Core logic for scanning, validating, and rewriting unit references
- `config/stack.go`: Integration with stack generation

Key functions:
- `scanExpressionForUnitReferences()`: Detects `unit.<name>.<output>` patterns
- `rewriteExpressionBytes()`: Transforms `unit.*` to `dependency.*.outputs.*`
- `validateUnitReferences()`: Ensures referenced units exist
- `detectCyclicDependencies()`: DFS-based cycle detection
- `writeValuesWithDependencies()`: Generates `terragrunt.values.hcl` with dependencies

