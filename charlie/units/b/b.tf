variable "b" {
  description = "b"
  type        = string
}

resource "null_resource" "b" {}

output "b" {
  value = "${var.b}-output"
}


