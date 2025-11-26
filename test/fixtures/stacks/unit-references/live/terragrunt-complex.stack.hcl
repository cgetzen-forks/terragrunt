# Test fixture for unit references feature with more complex scenarios
# This tests multiple dependencies, nested references, and string interpolation

unit "base" {
  source = "../units/dep"
  path   = "base"
  values = {
    message = "Base message"
  }
}

unit "middle" {
  source = "../units/dep"
  path   = "middle"
  values = {
    message = "Middle: ${unit.base.result}"
  }
}

unit "top" {
  source = "../units/app"
  path   = "top"
  values = {
    dep_message = unit.middle.result
    own_message = "Top level with base: ${unit.base.result}"
  }
}

