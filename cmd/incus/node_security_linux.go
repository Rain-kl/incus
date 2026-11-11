package main

import nodesecurity "github.com/lxc/incus/v7/internal/server/agent/security"

func observeIncusCLI(args []string) error { return nodesecurity.ObserveCLI(args) }
