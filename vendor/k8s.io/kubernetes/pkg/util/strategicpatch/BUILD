package(default_visibility = ["//visibility:public"])

licenses(["notice"])

load(
    "@io_bazel_rules_go//go:def.bzl",
    "go_binary",
    "go_library",
    "go_test",
    "cgo_library",
)

go_library(
    name = "go_default_library",
    srcs = ["patch.go"],
    tags = ["automanaged"],
    deps = [
        "//pkg/client/typed/discovery:go_default_library",
        "//pkg/util/json:go_default_library",
        "//third_party/forked/golang/json:go_default_library",
        "//vendor:github.com/davecgh/go-spew/spew",
        "//vendor:github.com/ghodss/yaml",
    ],
)

go_test(
    name = "go_default_test",
    srcs = ["patch_test.go"],
    library = "go_default_library",
    tags = ["automanaged"],
    deps = [
        "//vendor:github.com/davecgh/go-spew/spew",
        "//vendor:github.com/ghodss/yaml",
    ],
)
