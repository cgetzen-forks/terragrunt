# Test fixture for unit references feature
# This tests that units can reference outputs from other units

unit "dep" {
  source = "../units/dep"
  path   = "dep"
  values = {
    message = "Hello from dep"
  }
}

unit "app" {
  source = "../units/app"
  path   = "app"
  values = {
    dep_message = unit.dep.result
    own_message = "Hello from app"
  }
}

