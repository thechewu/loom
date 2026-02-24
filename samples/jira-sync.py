#!/usr/bin/env python3
"""
jira-sync.py — Poll Jira for new issues and create loom beads.

This is a sample integration that watches a Jira project for issues with a
specific label (e.g., "loom") and creates corresponding beads so that loom's
supervisor automatically picks them up and assigns them to AI agents.

Requirements:
    pip install requests

Usage:
    # Set environment variables
    export JIRA_URL=https://yourcompany.atlassian.net
    export JIRA_EMAIL=you@company.com
    export JIRA_API_TOKEN=your-api-token
    export JIRA_PROJECT=PROJ
    export LOOM_WORKSPACE=my-project  # must match the git repo directory name

    # Run once (process new issues, then exit)
    python jira-sync.py

    # Poll continuously (check every 60 seconds)
    python jira-sync.py --poll 60

    # Dry run (show what would be created, don't create beads)
    python jira-sync.py --dry-run

How it works:
    1. Queries Jira for issues matching: project = PROJ AND labels = "loom" AND status = "To Do"
    2. Skips issues that already have a bead (tracked via a local state file)
    3. Creates a bead for each new issue with the required loom labels
    4. Optionally adds a Jira comment linking back to the bead ID
    5. Repeats on --poll interval (or exits after one pass)

The loom supervisor (running via `loom run`) picks up the new beads automatically.
"""

import argparse
import json
import os
import subprocess
import sys
import time
from pathlib import Path

try:
    import requests
except ImportError:
    print("Install requests: pip install requests", file=sys.stderr)
    sys.exit(1)


# --- Configuration ---

JIRA_URL = os.environ.get("JIRA_URL", "")
JIRA_EMAIL = os.environ.get("JIRA_EMAIL", "")
JIRA_API_TOKEN = os.environ.get("JIRA_API_TOKEN", "")
JIRA_PROJECT = os.environ.get("JIRA_PROJECT", "")
JIRA_LABEL = os.environ.get("JIRA_LABEL", "loom")  # Jira label that triggers sync
LOOM_WORKSPACE = os.environ.get("LOOM_WORKSPACE", "")

# Local file tracking which Jira issues already have beads
STATE_FILE = Path(".loom/jira-sync-state.json")

# Priority mapping: Jira priority name → loom priority (0=highest, 4=lowest)
PRIORITY_MAP = {
    "Highest": 0,
    "High": 1,
    "Medium": 2,
    "Low": 3,
    "Lowest": 4,
}


def check_config():
    """Validate required environment variables."""
    missing = []
    if not JIRA_URL:
        missing.append("JIRA_URL")
    if not JIRA_EMAIL:
        missing.append("JIRA_EMAIL")
    if not JIRA_API_TOKEN:
        missing.append("JIRA_API_TOKEN")
    if not JIRA_PROJECT:
        missing.append("JIRA_PROJECT")
    if not LOOM_WORKSPACE:
        missing.append("LOOM_WORKSPACE")
    if missing:
        print(f"Missing environment variables: {', '.join(missing)}", file=sys.stderr)
        sys.exit(1)


def load_state():
    """Load the set of Jira issue keys that already have beads."""
    if STATE_FILE.exists():
        data = json.loads(STATE_FILE.read_text())
        return data.get("synced", {})
    return {}


def save_state(synced):
    """Persist the synced issue mapping."""
    STATE_FILE.parent.mkdir(parents=True, exist_ok=True)
    STATE_FILE.write_text(json.dumps({"synced": synced}, indent=2))


def fetch_jira_issues():
    """Query Jira for issues labeled for loom that are ready to work on."""
    jql = f'project = {JIRA_PROJECT} AND labels = "{JIRA_LABEL}" AND status = "To Do"'
    url = f"{JIRA_URL}/rest/api/3/search"
    params = {
        "jql": jql,
        "fields": "summary,description,priority,issuetype",
        "maxResults": 50,
    }
    auth = (JIRA_EMAIL, JIRA_API_TOKEN)

    resp = requests.get(url, params=params, auth=auth, timeout=30)
    resp.raise_for_status()
    return resp.json().get("issues", [])


def extract_description(issue):
    """Convert Jira issue to a self-contained agent prompt.

    Customize this function to include whatever context your agents need.
    The description is the agent's entire prompt — make it specific.
    """
    key = issue["key"]
    summary = issue["fields"]["summary"]

    # Jira's ADF (Atlassian Document Format) description — extract plain text
    desc_field = issue["fields"].get("description")
    if desc_field and isinstance(desc_field, dict):
        # Simple ADF text extraction (handles basic paragraphs)
        paragraphs = []
        for block in desc_field.get("content", []):
            if block.get("type") == "paragraph":
                text = "".join(
                    node.get("text", "")
                    for node in block.get("content", [])
                    if node.get("type") == "text"
                )
                if text:
                    paragraphs.append(text)
        description = "\n\n".join(paragraphs)
    elif desc_field and isinstance(desc_field, str):
        description = desc_field
    else:
        description = ""

    # Build the agent prompt
    prompt = f"Jira: {key} — {summary}\n\n"
    if description:
        prompt += f"{description}\n\n"
    prompt += (
        "IMPORTANT: This task was created from a Jira issue. "
        "Make the minimal changes needed to address the issue. "
        "Do not refactor unrelated code."
    )
    return prompt


def create_bead(title, description, priority, dry_run=False):
    """Create a loom bead via the bd CLI. Returns the bead ID."""
    cmd = [
        "bd", "create", "--json",
        "--title", title,
        "--type", "work",
        "--priority", str(priority),
        "--label", "loom:work",
        "--label", "loom:pending",
        "--label", f"loom:ws:{LOOM_WORKSPACE}",
        "--description", description,
    ]

    if dry_run:
        print(f"  [dry-run] would run: bd create --title {title!r} --priority {priority}")
        return "dry-run"

    result = subprocess.run(cmd, capture_output=True, text=True, timeout=30)
    if result.returncode != 0:
        print(f"  ERROR creating bead: {result.stderr.strip()}", file=sys.stderr)
        return None

    # Parse the JSON output to get the bead ID
    try:
        issue = json.loads(result.stdout)
        return issue.get("id", "unknown")
    except json.JSONDecodeError:
        # Fall back to scanning output for the ID
        for line in result.stdout.splitlines():
            if line.startswith("lm-"):
                return line.strip()
        return "unknown"


def add_jira_comment(issue_key, bead_id):
    """Add a comment to the Jira issue linking to the bead."""
    url = f"{JIRA_URL}/rest/api/3/issue/{issue_key}/comment"
    auth = (JIRA_EMAIL, JIRA_API_TOKEN)
    body = {
        "body": {
            "type": "doc",
            "version": 1,
            "content": [{
                "type": "paragraph",
                "content": [{
                    "type": "text",
                    "text": f"Loom bead created: {bead_id}. Work will be picked up automatically by the next available agent.",
                }],
            }],
        }
    }

    try:
        resp = requests.post(url, json=body, auth=auth, timeout=30)
        resp.raise_for_status()
    except requests.RequestException as e:
        print(f"  Warning: could not add Jira comment to {issue_key}: {e}", file=sys.stderr)


def sync_once(dry_run=False, comment=True):
    """Run one sync pass: fetch Jira issues, create beads for new ones."""
    synced = load_state()
    issues = fetch_jira_issues()

    if not issues:
        print("No new Jira issues to sync.")
        return 0

    created = 0
    for issue in issues:
        key = issue["key"]
        if key in synced:
            continue

        summary = issue["fields"]["summary"]
        priority_name = (issue["fields"].get("priority") or {}).get("name", "Medium")
        priority = PRIORITY_MAP.get(priority_name, 2)

        title = f"{key}: {summary}"
        description = extract_description(issue)

        print(f"Creating bead for {key}: {summary} (priority {priority})")
        bead_id = create_bead(title, description, priority, dry_run=dry_run)

        if bead_id:
            synced[key] = bead_id
            created += 1

            if comment and not dry_run:
                add_jira_comment(key, bead_id)

    if not dry_run:
        save_state(synced)

    print(f"Synced {created} new issue(s).")
    return created


def main():
    parser = argparse.ArgumentParser(description="Sync Jira issues to loom beads")
    parser.add_argument("--poll", type=int, default=0,
                        help="Poll interval in seconds (0 = run once and exit)")
    parser.add_argument("--dry-run", action="store_true",
                        help="Show what would be created without creating beads")
    parser.add_argument("--no-comment", action="store_true",
                        help="Don't add comments back to Jira issues")
    args = parser.parse_args()

    check_config()

    if args.poll > 0:
        print(f"Polling Jira every {args.poll}s (Ctrl+C to stop)")
        while True:
            try:
                sync_once(dry_run=args.dry_run, comment=not args.no_comment)
                time.sleep(args.poll)
            except KeyboardInterrupt:
                print("\nStopped.")
                break
            except requests.RequestException as e:
                print(f"Jira API error: {e}", file=sys.stderr)
                time.sleep(args.poll)
    else:
        sync_once(dry_run=args.dry_run, comment=not args.no_comment)


if __name__ == "__main__":
    main()
