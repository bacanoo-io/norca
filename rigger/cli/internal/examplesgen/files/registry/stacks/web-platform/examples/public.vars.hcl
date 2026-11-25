# Values of the stack: "rigger stack validate" and "rigger stack publish"
# evaluate the stack with the files given with -f; a published stack keeps
# its value files in examples/.
#
#   rigger stack validate stacks/web-platform -f stacks/web-platform/examples/public.vars.hcl

web_count    = 3
web_size     = "medium"
hostname     = "shop.example.com"
cache_memory = 512
