#!/usr/bin/env python3
"""Generate go.offline.mod for building nomira without a Go module proxy.

Strategy: use a local moby/buildkit checkout as the single dependency source.
Every module in its vendor/modules.txt gets a go.mod written into the vendor
subdirectory (module path + go directive) and a `replace` to that directory.
Requires in buildkit's go.mod that are NOT vendored get empty stub modules so
the module graph loads without network. gopkg.in/yaml.v3 (not vendored by
buildkit) is expected as an extra local checkout.

Usage:
  gen_offline_deps.py BUILDKIT_DIR BUILDKIT_VERSION YAML_DIR STUB_DIR OUT_MODFILE
"""
import os
import re
import sys

GO_DIRECTIVE = "go 1.24\n"


def write_gomod(dirpath: str, module: str) -> None:
    os.makedirs(dirpath, exist_ok=True)
    p = os.path.join(dirpath, "go.mod")
    if not os.path.exists(p):
        with open(p, "w") as f:
            f.write(f"module {module}\n\n{GO_DIRECTIVE}")


def parse_modules_txt(path: str):
    """Yield (module, version) for every vendored module (replacement-aware)."""
    with open(path) as f:
        for line in f:
            if not line.startswith("# "):
                continue
            # "# mod v1.2.3" or "# mod v1 => other v2" or "# mod => ../dir"
            body = line[2:].strip()
            lhs = body.split("=>")[0].strip().split()
            if not lhs:
                continue
            mod = lhs[0]
            ver = lhs[1] if len(lhs) > 1 else "v0.0.0"
            yield mod, ver


def parse_requires(gomod_path: str):
    """Yield (module, version) from a go.mod's require directives."""
    text = open(gomod_path).read()
    for m in re.finditer(r"(?m)^require\s+\(\n(.*?)^\)", text, re.S):
        for ln in m.group(1).splitlines():
            ln = ln.split("//")[0].strip()
            parts = ln.split()
            if len(parts) >= 2:
                yield parts[0], parts[1]
    for m in re.finditer(r"(?m)^require\s+(\S+)\s+(\S+)\s*(//.*)?$", text):
        if m.group(1) != "(":
            yield m.group(1), m.group(2)


def main() -> int:
    bk_dir, bk_ver, yaml_dir, stub_dir, out = sys.argv[1:6]
    vendor = os.path.join(bk_dir, "vendor")
    modules = dict(parse_modules_txt(os.path.join(vendor, "modules.txt")))

    requires = [f"\tgithub.com/moby/buildkit {bk_ver}\n",
                "\tgopkg.in/yaml.v3 v3.0.1\n"]
    replaces = [f"\tgithub.com/moby/buildkit => {bk_dir}\n",
                f"\tgopkg.in/yaml.v3 => {yaml_dir}\n"]

    for mod, ver in sorted(modules.items()):
        d = os.path.join(vendor, mod)
        if not os.path.isdir(d):
            print(f"warn: vendored module without dir: {mod}", file=sys.stderr)
            continue
        write_gomod(d, mod)
        requires.append(f"\t{mod} {ver}\n")
        replaces.append(f"\t{mod} => {d}\n")

    # Stubs for buildkit requires that are not vendored (module graph only).
    for mod, ver in parse_requires(os.path.join(bk_dir, "go.mod")):
        if mod in modules or mod == "gopkg.in/yaml.v3":
            continue
        d = os.path.join(stub_dir, mod)
        write_gomod(d, mod)
        requires.append(f"\t{mod} {ver}\n")
        replaces.append(f"\t{mod} => {d}\n")

    with open(out, "w") as f:
        f.write("module github.com/Nomira-OS/nomira\n\n")
        f.write(GO_DIRECTIVE + "\n")
        f.write("require (\n" + "".join(sorted(set(requires))) + ")\n\n")
        f.write("replace (\n" + "".join(sorted(set(replaces))) + ")\n")
    print(f"wrote {out}: {len(set(replaces))} replaces")
    return 0


if __name__ == "__main__":
    sys.exit(main())
