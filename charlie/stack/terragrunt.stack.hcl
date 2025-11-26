unit "a" {
  source = "../units/a"
  path   = "a"
  values = {
    a = "a"
  }
  mock_outputs = {
    a = "mock-a"
  }
}

unit "b" {
  source = "../units/b"
  path = "b"
  values = {
    b = unit.a.a
  }
}
