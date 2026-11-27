"""Print the creation time of an OCI image layout and the newest file time
in its last layer. Used by hack/e2e.sh."""
import json
import os
import sys
import tarfile

root = sys.argv[1]


def blob(digest):
    return os.path.join(root, "blobs", *digest.split(":"))


with open(os.path.join(root, "index.json")) as f:
    index = json.load(f)
with open(blob(index["manifests"][0]["digest"])) as f:
    manifest = json.load(f)
with open(blob(manifest["config"]["digest"])) as f:
    config = json.load(f)
with tarfile.open(blob(manifest["layers"][-1]["digest"])) as t:
    newest = max(m.mtime for m in t.getmembers())
print(config["created"], newest)
