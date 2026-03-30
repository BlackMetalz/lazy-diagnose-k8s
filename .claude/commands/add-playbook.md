Create a new diagnosis playbook. Ask the user for:
1. Playbook name (e.g., "HighMemory", "NodePressure")
2. What pod condition/state triggers this playbook
3. Key signals/evidence to check

Then:
1. Create a new file in `internal/diagnosis/` following the pattern of existing playbooks (CrashLoop, Pending, Rollout)
2. Register the playbook in the engine's playbook selection logic (`internal/playbook/`)
3. Add hypothesis definitions with weighted signals in `internal/diagnosis/analyzer.go` pattern
4. Add test cases in the corresponding `_test.go` file
5. Run `make test` to verify

Reference existing playbooks for the scoring pattern: each hypothesis has signals with weights, and the analyzer scores them against collected evidence.
