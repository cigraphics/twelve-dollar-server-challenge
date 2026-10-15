
#!/bin/bash
set -euo pipefail

REPO="arjaythedev/twelve-dollar-server-challenge"
REMOTE="upstream"
FAILED_FILE="failed-prs.txt"
MERGED_FILE="merged-prs.txt"

# Verify dependencies
for cmd in git gh sort; do
    if ! command -v "$cmd" >/dev/null 2>&1; then
        echo "ERROR: Missing dependency: $cmd"
        exit 1
    fi
done

gh auth status >/dev/null 2>&1 || {
    echo "ERROR: Run 'gh auth login' first."
    exit 1
}

# Verify Git repository
git rev-parse --is-inside-work-tree >/dev/null

# Never merge directly into main
BRANCH=$(git symbolic-ref --short HEAD)

if [ "$BRANCH" = "main" ] || [ "$BRANCH" = "master" ]; then
    echo "ERROR: Create an integration branch first:"
    echo "  git switch -c integrate-open-prs"
    exit 1
fi

# Ensure working tree is clean
# Check only tracked files for changes
if ! git diff --quiet || ! git diff --cached --quiet; then
    echo "ERROR: Tracked files have uncommitted changes."
    exit 1
fi

# Ensure upstream remote exists
if ! git remote get-url "$REMOTE" >/dev/null 2>&1; then
    git remote add "$REMOTE" \
        "https://github.com/$REPO.git"
fi

echo "Fetching upstream..."
git fetch "$REMOTE"

echo "Discovering open pull requests..."

PRS=()

# Bash 3.2-compatible array population
while IFS= read -r PR; do
    if [ -n "$PR" ]; then
        PRS+=("$PR")
    fi
done < <(
    gh api --paginate \
        "repos/$REPO/pulls?state=open&per_page=100" \
        --jq '.[].number' | sort -n
)

echo "Found ${#PRS[@]} open PRs."

if [ "${#PRS[@]}" -eq 0 ]; then
    echo "No open pull requests found."
    exit 0
fi

MERGED=()
FAILED=()
SKIPPED=()

# Write reports outside the working tree
REPORT_DIR="${TMPDIR:-/tmp}"
FAILED_FILE="$REPORT_DIR/failed-prs.txt"
MERGED_FILE="$REPORT_DIR/merged-prs.txt"

: > "$FAILED_FILE"
: > "$MERGED_FILE"

for PR in "${PRS[@]}"; do
    echo
    echo "======================================"
    echo "Processing PR #$PR"
    echo "======================================"

    REF="refs/remotes/$REMOTE/pr/$PR"

    if ! git fetch "$REMOTE" \
        "pull/$PR/head:$REF"; then
        echo "Failed to fetch PR #$PR"
        FAILED+=("$PR")
        echo "$PR fetch_failed" >> "$FAILED_FILE"
        continue
    fi

    # Skip PR if its commits are already ancestors
    if git merge-base --is-ancestor "$REF" HEAD; then
        echo "Already merged: PR #$PR"
        SKIPPED+=("$PR")
        continue
    fi

    # Attempt merge
    if git -c rerere.enabled=true merge \
        --no-ff \
        --no-edit \
        -m "Merge upstream PR #$PR" \
        "$REF"; then

        echo "Successfully merged PR #$PR"
        MERGED+=("$PR")
        echo "$PR" >> "$MERGED_FILE"

    else
        echo "Merge conflict in PR #$PR"

        # Abort only the failed merge
        git merge --abort || {
            echo "ERROR: Unable to abort merge."
            exit 1
        }

        FAILED+=("$PR")
        echo "$PR merge_conflict" >> "$FAILED_FILE"
    fi
done

echo
echo "======================================"
echo "FINAL SUMMARY"
echo "======================================"
echo "Total PRs:       ${#PRS[@]}"
echo "Merged:          ${#MERGED[@]}"
echo "Already merged:  ${#SKIPPED[@]}"
echo "Failed:          ${#FAILED[@]}"
echo
echo "Merged report: $MERGED_FILE"
echo "Failed report: $FAILED_FILE"
echo
echo "Current branch: $BRANCH"
echo
echo "Review changes:"
echo "  git log --oneline --graph -20"
echo "  git diff --stat $REMOTE/main...HEAD"
echo
echo "Push integration branch:"
echo "  git push -u origin $BRANCH"


