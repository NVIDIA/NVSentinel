#!/usr/bin/env python3
# Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Edits .openvex.json, the OpenVEX document of the NVSentinel images (see SECURITY.md).

  statements.py add ROWS.tsv       add statements from a TSV file
  statements.py prune SCAN.json... remove the packages that the scanned images no longer have
  statements.py format             rewrite the document in the repository format

Each row of ROWS.tsv has five tab-separated fields: the vulnerability ID that Grype reports, the
image (for example gpu-reset), the PURL of the package that Grype reports, the justification and
the impact statement. Lines that start with # are skipped. Rows with the same vulnerability and
justification become one statement, with one product for each image.

prune reads the Grype JSON of each image, from a scan without --only-fixed and without the document
(scan.sh writes it as NAME.json). It skips NAME.vex.json and other files, and only changes the products
of the scanned images.

Every command also formats the document and sets its version to one more than the version on the
base branch (--base-ref, default origin/main), so that a pull request increases it once.
"""

import argparse
import datetime
import json
import re
import subprocess
import sys

JUSTIFICATIONS = {
    "component_not_present",
    "vulnerable_code_not_present",
    "vulnerable_code_not_in_execute_path",
    "vulnerable_code_cannot_be_controlled_by_adversary",
    "inline_mitigations_already_exist",
}


def now():
    return datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def product_id(image):
    """pkg:oci/<name> for an image name, reference or product ID."""
    if image.startswith("pkg:oci/"):
        return image
    ref = re.sub(r"^(registry|docker|podman|oci-archive|docker-archive|oci-dir):", "", image)  # Grype schemes
    name = ref.rsplit("/", 1)[-1].split("@", 1)[0].split(":", 1)[0]
    return "pkg:oci/" + name


def load(path):
    with open(path, encoding="utf-8") as f:
        return json.load(f)


def dump(doc, path):
    """Writes the document with one line for the name of each vulnerability and for each product."""
    text = json.dumps(doc, indent=2)
    text = re.sub(r'"vulnerability": \{\n\s+"name": ("[^"]+")\n\s+\}', r'"vulnerability": {"name": \1}', text)
    for statement in doc["statements"]:
        for product in statement["products"]:
            pretty = "\n".join(
                ("        " + line) if i else line for i, line in enumerate(json.dumps(product, indent=2).split("\n"))
            )
            text = text.replace(pretty, json.dumps(product, separators=(", ", ": ")), 1)
    with open(path, "w", encoding="utf-8") as f:
        f.write(text + "\n")


def normalize(doc):
    """Sorts statements and subcomponents, and drops empty products and statements. Products keep their order."""
    kept = []
    for s in doc["statements"]:
        products = []
        for p in s["products"]:
            subs = sorted({c["@id"] for c in p.get("subcomponents", [])})
            if "subcomponents" in p and not subs:
                continue
            products.append({**p, **({"subcomponents": [{"@id": c} for c in subs]} if "subcomponents" in p else {})})
        if products:
            kept.append({**s, "products": products})
    kept.sort(key=lambda s: (s["vulnerability"]["name"], s.get("justification", "")))
    doc["statements"] = kept


def set_version(doc, path, base_ref, changed):
    """Sets the version to one more than on the base branch, once, and the timestamp when statements changed."""
    try:
        base = json.loads(subprocess.run(["git", "show", f"{base_ref}:{path}"], capture_output=True, check=True,
                                         text=True).stdout)
    except (subprocess.CalledProcessError, json.JSONDecodeError, FileNotFoundError):
        print(f"warning: cannot read {path} from {base_ref}; the version is unchanged", file=sys.stderr)
        if changed:
            doc["timestamp"] = now()
        return
    differs = base.get("statements") != doc["statements"]
    if differs and doc.get("version", 0) <= base.get("version", 0):
        doc["version"] = base.get("version", 0) + 1
    if changed or (differs and doc.get("timestamp") == base.get("timestamp")):
        doc["timestamp"] = now()


def add(doc, rows_path):
    changed = 0
    with open(rows_path, encoding="utf-8") as f:
        for n, line in enumerate(f, 1):
            line = line.rstrip("\n")
            if not line.strip() or line.startswith("#"):
                continue
            fields = line.split("\t")
            if len(fields) != 5:
                sys.exit(f"{rows_path}:{n}: expected 5 tab-separated fields, found {len(fields)}")
            vuln, image, purl, justification, impact = (x.strip() for x in fields)
            if justification not in JUSTIFICATIONS:
                sys.exit(f"{rows_path}:{n}: unknown justification {justification!r}")
            statement = next((s for s in doc["statements"] if s["vulnerability"]["name"] == vuln
                              and s.get("justification") == justification), None)
            if statement is None:
                statement = {"vulnerability": {"name": vuln}, "timestamp": now(), "products": [],
                             "status": "not_affected", "justification": justification, "impact_statement": impact}
                doc["statements"].append(statement)
            pid = product_id(image)
            product = next((p for p in statement["products"] if p["@id"] == pid), None)
            if product is None:
                product = {"@id": pid, "subcomponents": []}
                statement["products"].append(product)
            if "subcomponents" not in product:
                print(f"{rows_path}:{n}: {vuln} already covers every package of {pid}; row skipped")
                continue
            if any(c["@id"] == purl for c in product["subcomponents"]) and impact in statement["impact_statement"]:
                continue
            if not any(c["@id"] == purl for c in product["subcomponents"]):
                product["subcomponents"].append({"@id": purl})
            if impact not in statement["impact_statement"]:
                statement["impact_statement"] += " " + impact
            statement["timestamp"] = now()
            changed += 1
    print(f"added or updated {changed} rows")
    return changed > 0


def prune(doc, scans):
    present = {}
    for path in scans:
        if path.endswith(".vex.json"):
            print(f"skip {path}: a scan with the document leaves out findings without a fix")
            continue
        scan = load(path)
        if "matches" not in scan or "source" not in scan:
            print(f"skip {path}: not a Grype JSON report")
            continue
        pid = product_id(scan["source"]["target"]["userInput"])
        found = present.setdefault(pid, set())
        for m in scan.get("matches", []) + scan.get("ignoredMatches", []):
            found.add((m["vulnerability"]["id"], m["artifact"]["purl"]))
    removed = 0
    for s in doc["statements"]:
        for p in s["products"]:
            if p["@id"] not in present or "subcomponents" not in p:
                continue
            keep = [c for c in p["subcomponents"] if (s["vulnerability"]["name"], c["@id"]) in present[p["@id"]]]
            for c in p["subcomponents"]:
                if c not in keep:
                    print(f"remove {s['vulnerability']['name']} {p['@id']} {c['@id']}")
                    removed += 1
            p["subcomponents"] = keep
    print(f"removed {removed} subcomponents; scanned {', '.join(sorted(present))}")
    return removed > 0


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--file", default=".openvex.json")
    parser.add_argument("--base-ref", default="origin/main")
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("add").add_argument("rows")
    sub.add_parser("prune").add_argument("scans", nargs="+")
    sub.add_parser("format")
    args = parser.parse_args()

    doc = load(args.file)
    changed = False
    if args.command == "add":
        changed = add(doc, args.rows)
    elif args.command == "prune":
        changed = prune(doc, args.scans)
    normalize(doc)
    set_version(doc, args.file, args.base_ref, changed)
    dump(doc, args.file)


if __name__ == "__main__":
    main()
