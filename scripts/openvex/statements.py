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

  statements.py add ROWS.tsv        add statements from a TSV file
  statements.py prune SCAN.json...  remove the packages that the scanned images no longer have
  statements.py format              rewrite the document in the repository format

add: each row of ROWS.tsv has five tab-separated fields: the vulnerability ID that Grype reports, the
image, the PURL that Grype reports, the justification and the impact statement. Lines that start with #
are skipped.
For example:

  CVE-2026-86138  gpu-reset  pkg:deb/ubuntu/libxml2@2.9.14?arch=amd64&distro=ubuntu-24.04  vulnerable_code_not_in_execute_path  libxml2 is a library of the base image that no NVSentinel process loads.

Rows with the same vulnerability and justification become one statement, with one product for each
image. The statements are sorted by vulnerability ID.

prune: reads the Grype JSON of each image, from a scan without --only-fixed and without the document.
scan.sh writes it as NAME.json. prune skips NAME.vex.json and other files, and only changes the products
of the scanned images.

Every command also formats the document and sets its version to one more than the version on the base
branch (--base-ref, default origin/main), so that a pull request increases it once.
"""

import argparse
import datetime
import json
import re
import subprocess
import sys
from collections import namedtuple

JUSTIFICATIONS = {
    "component_not_present",
    "vulnerable_code_not_present",
    "vulnerable_code_not_in_execute_path",
    "vulnerable_code_cannot_be_controlled_by_adversary",
    "inline_mitigations_already_exist",
}

# The prefixes that Grype accepts before an image reference, for example registry:ghcr.io/...
GRYPE_SCHEMES = r"^(registry|docker|podman|oci-archive|docker-archive|oci-dir):"

Row = namedtuple("Row", "line vulnerability product purl justification impact")


def utc_now():
    """Returns the current time in the format of the document, for example "2026-11-16T07:03:21Z"."""
    return datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def product_id_for_image(image):
    """Returns the OpenVEX product ID of an image: pkg:oci/ and the last part of its repository.

    The ID has no tag, so a statement applies to every tag of the image. Examples, which all give
    "pkg:oci/gpu-reset":

      gpu-reset
      ghcr.io/nvidia/nvsentinel/gpu-reset:main-6e388be
      registry:ghcr.io/nvidia/nvsentinel/gpu-reset@sha256:...
      docker:gpu-reset:latest
      pkg:oci/gpu-reset
    """
    if image.startswith("pkg:oci/"):
        return image
    reference = re.sub(GRYPE_SCHEMES, "", image)
    name = reference.rsplit("/", 1)[-1].split("@", 1)[0].split(":", 1)[0]
    return "pkg:oci/" + name


def read_json(path):
    with open(path, encoding="utf-8") as f:
        return json.load(f)


def write_document(doc, path):
    """Writes the document as indented JSON, with one line for each vulnerability name and each product.

    For example, a statement is written as:

      {
        "vulnerability": {"name": "CVE-2026-86138"},
        "timestamp": "2026-11-16T00:00:00Z",
        "products": [
          {"@id": "pkg:oci/gpu-reset", "subcomponents": [{"@id": "pkg:deb/ubuntu/libxml2@2.9.14?arch=amd64&distro=ubuntu-24.04"}]}
        ],
        "status": "not_affected",
        ...
      }
    """
    text = json.dumps(doc, indent=2)
    text = re.sub(r'"vulnerability": \{\n\s+"name": ("[^"]+")\n\s+\}', r'"vulnerability": {"name": \1}', text)
    for statement in doc["statements"]:
        for product in statement["products"]:
            # json.dumps(indent=2) of the product, as it appears at its depth in the document
            indented = "\n".join(
                ("        " + line) if i else line for i, line in enumerate(json.dumps(product, indent=2).split("\n"))
            )
            text = text.replace(indented, json.dumps(product, separators=(", ", ": ")), 1)
    with open(path, "w", encoding="utf-8") as f:
        f.write(text + "\n")


def read_rows(path):
    """Returns the rows of a TSV file of findings, and stops at the first row that is not valid.

    Example: the line
      "CVE-2026-86138<TAB>gpu-reset<TAB>pkg:deb/ubuntu/libxml2@2.9.14<TAB>vulnerable_code_not_in_execute_path<TAB>libxml2 ..."
    gives Row(vulnerability="CVE-2026-86138", product="pkg:oci/gpu-reset", purl="pkg:deb/ubuntu/libxml2@2.9.14", ...).
    """
    rows = []
    with open(path, encoding="utf-8") as f:
        for number, line in enumerate(f, 1):
            line = line.rstrip("\n")
            if not line.strip() or line.startswith("#"):
                continue
            fields = [field.strip() for field in line.split("\t")]
            if len(fields) != 5:
                sys.exit(f"{path}:{number}: expected 5 tab-separated fields, found {len(fields)}")
            vulnerability, image, purl, justification, impact = fields
            if justification not in JUSTIFICATIONS:
                sys.exit(f"{path}:{number}: unknown justification {justification!r}")
            rows.append(Row(f"{path}:{number}", vulnerability, product_id_for_image(image), purl, justification, impact))
    return rows


def statement_for(doc, vulnerability, justification, impact):
    """Returns the statement of the vulnerability with the justification, and adds one if there is none.

    A vulnerability can have one statement for each justification, for example GO-2026-6611 with
    vulnerable_code_not_in_execute_path for some images and vulnerable_code_cannot_be_controlled_by_adversary
    for another.
    """
    for statement in doc["statements"]:
        if statement["vulnerability"]["name"] == vulnerability and statement.get("justification") == justification:
            return statement
    statement = {"vulnerability": {"name": vulnerability}, "timestamp": utc_now(), "products": [],
                 "status": "not_affected", "justification": justification, "impact_statement": impact}
    doc["statements"].append(statement)
    return statement


def product_in(statement, product_id):
    """Returns the product of the statement with the ID, and adds one, with no packages yet, if there is none."""
    for product in statement["products"]:
        if product["@id"] == product_id:
            return product
    product = {"@id": product_id, "subcomponents": []}
    statement["products"].append(product)
    return product


def add_rows(doc, rows):
    """Adds each row to the statement of its vulnerability and justification. Returns True if anything changed.

    Example: rows for CVE-2026-86138 in gpu-reset and in metadata-collector, with the same justification,
    give one statement with the products pkg:oci/gpu-reset and pkg:oci/metadata-collector. A row whose
    impact statement differs from the statement's adds it as another sentence.
    """
    changed = 0
    for row in rows:
        statement = statement_for(doc, row.vulnerability, row.justification, row.impact)
        product = product_in(statement, row.product)
        if "subcomponents" not in product:
            print(f"{row.line}: {row.vulnerability} already covers every package of {row.product}; row skipped")
            continue
        has_package = any(c["@id"] == row.purl for c in product["subcomponents"])
        has_reason = row.impact in statement["impact_statement"]
        if has_package and has_reason:
            continue
        if not has_package:
            product["subcomponents"].append({"@id": row.purl})
        if not has_reason:
            statement["impact_statement"] += " " + row.impact
        statement["timestamp"] = utc_now()
        changed += 1
    print(f"added or updated {changed} rows")
    return changed > 0


def findings_in_scans(paths):
    """Returns, for each scanned image, the (vulnerability, PURL) pairs that its Grype JSON reports.

    Example: a scan of ghcr.io/nvidia/nvsentinel/gpu-reset:main-6e388be that reports CVE-2026-82560 in
    perl-base gives {"pkg:oci/gpu-reset": {("CVE-2026-82560", "pkg:deb/ubuntu/perl-base@5.38.2-...")}}.
    Scans of two tags of the same image add up, for example the DCGM 3.x and 4.x tags of gpu-health-monitor.
    """
    findings = {}
    for path in paths:
        if path.endswith(".vex.json"):
            print(f"skip {path}: a scan with the document leaves out findings without a fix")
            continue
        scan = read_json(path)
        if "matches" not in scan or "source" not in scan:
            print(f"skip {path}: not a Grype JSON report")
            continue
        image = findings.setdefault(product_id_for_image(scan["source"]["target"]["userInput"]), set())
        for match in scan.get("matches", []) + scan.get("ignoredMatches", []):
            image.add((match["vulnerability"]["id"], match["artifact"]["purl"]))
    return findings


def remove_packages_not_found(doc, findings):
    """Removes each package from a statement when the scan of its image no longer reports the vulnerability for it.

    Example: if a statement covers CVE-2026-82560 in perl-base 5.38.2-3.2ubuntu0.6 for gpu-reset, and the
    new scan of gpu-reset reports perl-base 5.38.2-3.2ubuntu0.7 instead, the 0.6 package is removed. The
    products of images that were not scanned do not change. Returns True if anything was removed.
    """
    removed = 0
    for statement in doc["statements"]:
        vulnerability = statement["vulnerability"]["name"]
        for product in statement["products"]:
            if product["@id"] not in findings or "subcomponents" not in product:
                continue
            kept = []
            for package in product["subcomponents"]:
                if (vulnerability, package["@id"]) in findings[product["@id"]]:
                    kept.append(package)
                else:
                    print(f"remove {vulnerability} {product['@id']} {package['@id']}")
                    removed += 1
            product["subcomponents"] = kept
    print(f"removed {removed} subcomponents; scanned {', '.join(sorted(findings))}")
    return removed > 0


def sort_and_drop_empty(doc):
    """Sorts the statements by vulnerability and justification, and the packages of each product by PURL.

    Drops a product that has no packages left, and a statement that has no products left. A product
    without a subcomponents field covers every package of the image, so it stays. Products keep their order.
    """
    kept = []
    for statement in doc["statements"]:
        products = []
        for product in statement["products"]:
            if "subcomponents" not in product:
                products.append(product)
                continue
            packages = sorted({c["@id"] for c in product["subcomponents"]})
            if packages:
                products.append({**product, "subcomponents": [{"@id": purl} for purl in packages]})
        if products:
            kept.append({**statement, "products": products})
    kept.sort(key=lambda s: (s["vulnerability"]["name"], s.get("justification", "")))
    doc["statements"] = kept


def document_on_branch(path, ref):
    """Returns the document as it is on the base branch, or None if git cannot read it there."""
    try:
        return json.loads(subprocess.run(["git", "show", f"{ref}:{path}"], capture_output=True, check=True,
                                         text=True).stdout)
    except (subprocess.CalledProcessError, json.JSONDecodeError, FileNotFoundError):
        return None


def update_version_and_timestamp(doc, base, changed):
    """Sets the version to one more than on the base branch, once, and the timestamp when statements changed.

    Example: on a branch from main, where the version is 2, the first change sets 3, and later runs keep 3.
    A document that does not differ from main keeps version 2. The timestamp changes when this run changed
    statements, or when the statements differ from main but the timestamp is still the one of main, for
    example after an edit by hand.
    """
    if base is None:
        print("warning: cannot read the document on the base branch; the version is unchanged", file=sys.stderr)
        if changed:
            doc["timestamp"] = utc_now()
        return
    differs = base.get("statements") != doc["statements"]
    if differs and doc.get("version", 0) <= base.get("version", 0):
        doc["version"] = base.get("version", 0) + 1
    if changed or (differs and doc.get("timestamp") == base.get("timestamp")):
        doc["timestamp"] = utc_now()


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--file", default=".openvex.json")
    parser.add_argument("--base-ref", default="origin/main")
    commands = parser.add_subparsers(dest="command", required=True)
    commands.add_parser("add").add_argument("rows")
    commands.add_parser("prune").add_argument("scans", nargs="+")
    commands.add_parser("format")
    args = parser.parse_args()

    doc = read_json(args.file)
    changed = False
    if args.command == "add":
        changed = add_rows(doc, read_rows(args.rows))
    elif args.command == "prune":
        changed = remove_packages_not_found(doc, findings_in_scans(args.scans))
    sort_and_drop_empty(doc)
    update_version_and_timestamp(doc, document_on_branch(args.file, args.base_ref), changed)
    write_document(doc, args.file)


if __name__ == "__main__":
    main()
