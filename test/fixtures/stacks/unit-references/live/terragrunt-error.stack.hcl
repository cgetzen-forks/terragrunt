# Test fixture for error handling - references non-existent unit

unit "app" {
  source = "../units/app"
  path   = "app"
  values = {
    dep_message = unit.nonexistent.result
    own_message = "Hello from app"
  }
}

