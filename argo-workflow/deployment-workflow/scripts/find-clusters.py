# Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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
import logging
import os
import subprocess
import sys
import json
import tempfile
from collections import defaultdict
from pathlib import Path
from typing import List, Dict, Any, Optional
import yaml
import re

# Configure logging
logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s [%(levelname)s] %(message)s',
    datefmt='%Y-%m-%d %H:%M:%S'
)
logger = logging.getLogger('find-clusters')

def run_command(cmd: List[str], cwd: Optional[str] = None, check: bool = True) -> str:
    """Run a command and return its output"""
    try:
        process = subprocess.run(
            cmd,
            cwd=cwd,
            capture_output=True,
            text=True,
            check=check
        )
        return process.stdout.strip()
    except subprocess.CalledProcessError as e:
        logger.error(f"Error running command: {' '.join(cmd)}")
        logger.error(f"Error output: {e.stderr}")
        if check:
            raise
        return ""

def main():
    
    version = os.environ.get('VERSION')
    gitlab_token = os.environ.get('MANIFEST_GITLAB_TOKEN')
    gitlab_url = os.environ.get('GITLAB_URL')

    if not all([version, gitlab_token, gitlab_url]):
        logger.error("ERROR:Missing required environment variables")
        sys.exit(1)

    logger.info(f"Start version={version}")

    def get_cluster_patterns() -> List[Dict[str, Any]]:
        """
        Load cluster patterns from the mounted ConfigMap volume.
        """
        config_path = '/cluster-patterns/clusters.yaml'

        try:
            if not os.path.isfile(config_path):
                logger.error(f"Config file {config_path} does not exist")
                return []

            with open(config_path, 'r') as f:
                yaml_content = f.read()

            config = yaml.safe_load(yaml_content)
            patterns = config.get('patterns', [])
            return patterns

        except Exception as e:
            logger.error(f"Config load error: {e}")
            sys.exit(1)

    def find_cluster_specs(repo_dir: str) -> List[str]:
        """Find all cluster-spec.yaml files in the repository"""
        spec_files = []
        for path in Path(repo_dir).rglob('cluster-spec.yaml'):
            spec_files.append(str(path))
        return spec_files

    def match_cluster_to_pattern(
        cluster_name: str,
        include_pattern: str,
        exclude_pattern: Optional[str]
    ) -> bool:
        """Check if a cluster name matches the include/exclude patterns"""
        if not include_pattern:
            return False

        # Convert glob to regex
        include_regex = include_pattern.replace('*', '.*')
        exclude_regex = exclude_pattern.replace('*', '.*') if exclude_pattern else None

        # Check if cluster matches
        if re.match(include_regex, cluster_name):
            if not exclude_regex or not re.match(exclude_regex, cluster_name):
                return True
        return False

    def process_cluster_spec(
        spec_file: str,
        patterns: List[Dict[str, Any]],
        repo_dir: str
    ) -> Optional[Dict[str, Any]]:
        """Process a single cluster spec file and match it against patterns"""
        try:
            with open(spec_file, 'r') as f:
                spec = yaml.safe_load(f)

            cluster_name = spec.get('metadata', {}).get('name')
            if not cluster_name:
                return None

            # Check against each pattern
            for pattern in patterns:
                include_pattern = pattern.get('include', '')
                exclude_pattern = pattern.get('exclude', '')

                if match_cluster_to_pattern(cluster_name, include_pattern, exclude_pattern):
                    relative_path = os.path.relpath(spec_file, repo_dir)
                    pattern_name = pattern.get('name', 'Unknown')
                    logger.info(f"Match {cluster_name} → {pattern_name}")
                    
                    return {
                        'pattern_name': pattern_name,
                        'cluster_info': {
                            'name': cluster_name,
                            'spec_file_path': relative_path
                        }
                    }

        except Exception as e:
            logger.error(f"Error reading {spec_file}: {e}")

        return None

    # Get patterns from ConfigMap
    patterns = get_cluster_patterns()
    if not patterns:
        return

    # Group clusters by pattern
    pattern_groups = defaultdict(list)

    # Clone and process repository
    repo_url = f"{gitlab_url}/dgxcloud/mk8s/manifests.git"
    auth_url = repo_url.replace('https://', f'https://oauth2:{gitlab_token}@')
    logger.info("Cloning manifests repo...")

    with tempfile.TemporaryDirectory() as temp_dir:
        # Clone repository
        try:
            run_command(['git', 'clone', '--depth', '1', auth_url, temp_dir])
        except Exception as e:
            logger.error(f"Clone failed: {e}")
            sys.exit(1)

        # Find and process cluster specs
        spec_files = find_cluster_specs(temp_dir)
        logger.info(f"Found {len(spec_files)} cluster-spec.yaml files")

        for spec_file in spec_files:
            result = process_cluster_spec(spec_file, patterns, temp_dir)
            if result:
                pattern_name = result['pattern_name']
                cluster_info = result['cluster_info']
                pattern_groups[pattern_name].append(cluster_info)

    # Build final output structure
    result = []
    for pattern in patterns:
        pattern_name = pattern.get('name', 'Unknown')
        if pattern_name in pattern_groups:
            pattern_data = {
                'pattern_name': pattern_name,
                'pattern_info': {
                    'version': version,
                    'spec': pattern.get('spec', {})
                },
                'clusters': pattern_groups[pattern_name]
            }
            result.append(pattern_data)
            logger.info(f"Pattern {pattern_name}: {len(pattern_groups[pattern_name])} clusters")

    logger.info(f"Total patterns with matches: {len(result)}")

    # Save results
    with open('/tmp/clusters.json', 'w') as f:
        json.dump(result, f, indent=2)

if __name__ == '__main__':
    main()