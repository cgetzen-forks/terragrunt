variable "a" {
  description = "a"
  type        = string
}

resource "null_resource" "a" {}

output "a" {
  value = "${var.a}-output"
}


