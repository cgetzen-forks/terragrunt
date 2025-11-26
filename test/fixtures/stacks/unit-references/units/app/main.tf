variable "dep_message" {
  description = "Message from dependency"
  type        = string
}

variable "own_message" {
  description = "Own message"
  type        = string
}

resource "local_file" "file" {
  content  = "${var.own_message} - ${var.dep_message}"
  filename = "${path.module}/data.txt"
}

output "combined" {
  value = "${var.own_message} - ${var.dep_message}"
}

