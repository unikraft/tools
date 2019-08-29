// Copyright 2019 The UNICORE Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file
//
// Author: Gaulthier Gain <gaulthier.gain@uliege.be>

package dependency_analysis_tool

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	u "github.com/unikraft/tools/utils_toolchain"

	"github.com/unikraft/tools/dependency_analysis_tool/utils_dependency"
)

type DynamicArgs struct {
	waitTime                     int
	verbose, display, background bool
	options, testFile            string
}

// --------------------------------Gather Data----------------------------------

// gatherDynamicData gathers symbols and system calls of a given application
// which is NOT a background process.
//
// It returns an error if any, otherwise it returns nil.
func gatherDynamicData(command, programPath string, data *u.DynamicData) error {

	if output, err := u.ExecuteCommand(command, []string{"-f",
		programPath}); err != nil {
		return err
	} else {
		if data.SystemCalls == nil {
			data.SystemCalls = make(map[string]string)
			utils_dependency.ParseTrace(output, data.SystemCalls)
		} else {
			data.Symbols = make(map[string]string)
			utils_dependency.ParseTrace(output, data.Symbols)
		}
	}

	return nil
}

// gatherDynamicDataBackground gathers symbols and system calls of a given
// application which is a background process.
//
func gatherDynamicDataBackground(command, programPath, programName string,
	data *u.DynamicData, dArgs DynamicArgs) {

	_, errStr := CaptureOutput(programPath, programName, command, dArgs, data)

	if data.SystemCalls == nil {
		data.SystemCalls = make(map[string]string)
		utils_dependency.ParseTrace(errStr, data.SystemCalls)
	} else {
		data.Symbols = make(map[string]string)
		utils_dependency.ParseTrace(errStr, data.Symbols)
	}
}

// gatherDynamicSharedLibs gathers shared libraries of a given application.
//
// It returns an error if any, otherwise it returns nil.
func gatherDynamicSharedLibs(programName string, pid int, data *u.DynamicData,
	v bool) error {

	// Get the pid
	pidStr := strconv.Itoa(pid)
	u.PrintInfo("PID '" + programName + "' : " + pidStr)

	// Use 'lsof' to get open files and thus .so files
	if output, err := u.ExecutePipeCommand(
		"lsof -p " + pidStr + " | uniq | awk '{print $9}'"); err != nil {
		return err
	} else {
		// Parse 'lsof' output
		if err := utils_dependency.ParseLsof(output, data, v); err != nil {
			u.PrintErr(err)
		}
	}

	// Use 'cat /proc/pid' to get open files and thus .so files
	if output, err := u.ExecutePipeCommand(
		"cat /proc/" + pidStr + "/maps | awk '{print $6}' | " +
			"grep '\\.so' | sort | uniq"); err != nil {
		return err
	} else {
		// Parse 'cat' output
		if err := utils_dependency.ParseLsof(output, data, v); err != nil {
			u.PrintErr(err)
		}
	}

	return nil
}

// -----------------------------------TESTER------------------------------------

// launchTests runs external tests written in the 'test.txt' file.
//
// It returns an error if any, otherwise it returns nil.
func launchTests(args DynamicArgs) error {
	file, err := os.Open(args.testFile)
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Split(bufio.ScanLines)

	for scanner.Scan() {
		cmd := scanner.Text()
		if len(cmd) > 0 {
			// Execute each line as a command
			if _, err := u.ExecutePipeCommand(cmd); err != nil {
				u.PrintWarning("Impossible to execute test: " + cmd)
			}
		}
	}

	return nil
}

// CaptureOutput captures stdout and stderr of a the executed command. It will
// also run the Tester to explore several execution paths of the given app.
//
// It returns to string which are respectively stdout and stderr.
func CaptureOutput(programPath, programName, command string,
	dArgs DynamicArgs, data *u.DynamicData) (string, string) {

	args := strings.Fields("-f " + programPath + " " + dArgs.options)
	cmd := exec.Command(command, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	bufOut, bufErr := &bytes.Buffer{}, &bytes.Buffer{}
	cmd.Stdout = io.MultiWriter(bufOut) // Add os.Stdin to record on stdout
	cmd.Stderr = io.MultiWriter(bufErr) // Add os.Stdin to record on stderr
	cmd.Stdin = os.Stdin

	if err := cmd.Start(); err != nil {
		log.Printf("Failed to start cmd: %v", err)
		u.PrintErr(err)
	}

	u.PrintInfo("Run '" + programName + "' in background")
	Tester(programName, cmd.Process, data, dArgs)

	// Add timeout if program is not killed
	var canceled = make(chan struct{})
	timeoutKill := time.NewTimer(time.Second)
	go func() {
		select {
		case <-timeoutKill.C:
			if err := u.PKill(programName, syscall.SIGINT); err != nil {
				u.PrintErr(err)
			}
		case <-canceled:
		}
	}()

	// Ignore the error because the program is killed (waitTime)
	_ = cmd.Wait()

	// Add timeout if program is not killed
	select {
	case canceled <- struct{}{}:
	default:
	}
	timeoutKill.Stop()

	return bufOut.String(), bufErr.String()
}

// Tester runs the executable file of a given application to perform tests to
// get program dependencies
//
func Tester(programName string, process *os.Process, data *u.DynamicData,
	dArgs DynamicArgs) {

	if len(dArgs.testFile) > 0 {
		u.PrintInfo("Run internal tests from file " + dArgs.testFile)

		// Wait until the program has started
		time.Sleep(time.Second * 5)

		if err := launchTests(dArgs); err != nil {
			u.PrintWarning(err)
		}
	} else {
		u.PrintInfo("Waiting for external tests for " + strconv.Itoa(
			dArgs.waitTime) + " sec")
		ticker := time.Tick(time.Second)
		for i := 1; i <= dArgs.waitTime; i++ {
			<-ticker
			fmt.Printf("-")
		}
		fmt.Printf("\n")
	}

	// Gather shared libs
	u.PrintHeader2("(*) Gathering shared libs")
	if err := gatherDynamicSharedLibs(programName, process.Pid, data,
		dArgs.verbose); err != nil {
		u.PrintWarning(err)
	}

	// Kill process after elapsed time
	u.PrintInfo("Kill '" + programName + "'")
	if err := process.Kill(); err != nil {
		u.PrintErr(err)
	} else {
		u.PrintOk("'" + programName + "' Killed")
	}
}

// ------------------------------------ARGS-------------------------------------

// getDArgs returns a DynamicArgs struct which contains arguments specific to
// the dynamic dependency analysis
//
// It returns two strings which are respectively stdout and stderr.
func getDArgs(args u.Arguments) DynamicArgs {
	return DynamicArgs{*args.IntArg["waitTime"],
		*args.BoolArg["verbose"], *args.BoolArg["display"],
		*args.BoolArg["background"], *args.StringArg["options"],
		*args.StringArg["testFile"]}
}

// -------------------------------------RUN-------------------------------------

// RunDynamicAnalyser runs the dynamic analysis to get shared libraries,
// system calls and library calls of a given application.
//
func RunDynamicAnalyser(args u.Arguments, data *u.Data, programPath,
	outFolder string) {

	// Get dynamic structure
	dArgs := getDArgs(args)
	programName := *args.StringArg["program"]

	// Kill process if it is already launched
	u.PrintInfo("Kill '" + programName + "' if it is already launched")
	if err := u.PKill(programName, syscall.SIGINT); err != nil {
		u.PrintErr(err)
	}

	dynamicData := &data.DynamicData

	u.PrintHeader2("(*) Gathering system call from ELF file")
	dynamicData.SharedLibs = make(map[string][]string)
	if dArgs.background {
		gatherDynamicDataBackground("strace", programPath, programName,
			dynamicData, dArgs)
	} else {
		if err := gatherDynamicData("strace", programPath,
			dynamicData); err != nil {
			u.PrintErr(err)
		}
	}

	u.PrintHeader2("(*) Gathering symbols from ELF file")
	if dArgs.background {
		gatherDynamicDataBackground("ltrace", programPath, programName,
			dynamicData, dArgs)
	} else {
		if err := gatherDynamicData("ltrace", programPath,
			dynamicData); err != nil {
			u.PrintErr(err)
		}
	}

	// Create the folder 'output/dynamic' if it does not exist
	if _, err := os.Stat(outFolder); os.IsNotExist(err) {
		if err = os.Mkdir(outFolder, os.ModePerm); err != nil {
			u.PrintErr(err)
		}
	}
}
