# Test fixture for cyclic dependency detection

unit "a" {
  source = "../units/dep"
  path   = "a"
  values = {
    message = unit.b.result
  }
}

unit "b" {
  source = "../units/dep"
  path   = "b"
  values = {
    message = unit.a.result
  }
}

