package client

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/NHAS/reverse_ssh/internal"
	"github.com/NHAS/reverse_ssh/internal/server/users"
	"golang.org/x/crypto/ssh"
)

func scpChannel(user *users.User, newChannel ssh.NewChannel) {
	connection, requests, err := newChannel.Accept()
	if err != nil {
		log.Printf("Could not accept channel (%s)", err)
		return
	}
	defer connection.Close()
	go ssh.DiscardRequests(requests)

	var scpInfo internal.Scp

	err = ssh.Unmarshal(newChannel.ExtraData(), &scpInfo)
	if err != nil {
		log.Printf("Unable to unmarshal scpInfo (%s)", err)
		return
	}

	log.Println("Mode: ", scpInfo.Mode, scpInfo.Path)
	switch scpInfo.Mode {
	case "-t":
		to(scpInfo.Path, connection)
	case "-f":
		from(scpInfo.Path, connection)
	default:
		log.Println("Unknown mode.")
	}

}

func readProtocolControl(connection ssh.Channel) (string, uint32, uint64, string) {
	control, err := bufio.NewReader(connection).ReadString('\n')
	if err != nil {
		log.Println(err)
	}

	connection.Write([]byte{0})

	parts := strings.SplitN(control, " ", 3)
	if len(parts) != 3 {
		return "", 0, 0, ""
	}

	mode, _ := strconv.ParseInt(parts[0][1:], 8, 32)
	size, _ := strconv.ParseInt(parts[1], 10, 64)
	filename := parts[len(parts)-1]
	filename = filename[:len(filename)-1]

	switch parts[0][0] {
	case 'D':
		return "dir", uint32(mode), uint64(size), filename

	case 'C':
		return "file", uint32(mode), uint64(size), filename
	case 'E':
		return "exit", 0, 0, ""

	default:
		log.Println("Unknown mode: ", strings.TrimSpace(control))
	}
	return "", 0, 0, ""
}

func readFile(connection ssh.Channel, path string, mode uint32, size uint64) error {

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, fs.FileMode(mode))
	if err != nil {

		return err
	}
	defer f.Close()

	b := make([]byte, 1024)
	var nn uint64
	for {

		n, err := connection.Read(b[:size%1024])
		if err != nil {
			return err
		}

		nn += uint64(n)

		nf, err := f.Write(b[:n])
		if err != nil {

			return err
		}

		if nf != n {
			return err
		}

		if nn == size {
			break
		}
	}

	status, _ := readAck(connection)
	if status != 0 {
		return errors.New("ACK bad")
	}
	connection.Write([]byte{0})

	return nil
}

func readDirectory(connection ssh.Channel, path string, mode uint32) {

	err := os.Mkdir(path, os.FileMode(mode))
	if err != nil && !os.IsExist(err) {
		return
	}

	t, mode, size, filename := readProtocolControl(connection)
	log.Printf("%s %#o %d %s\n", t, mode, size, filename)

	newPath := filepath.Join(path, filename)
	for {
		switch t {
		case "dir":
			readDirectory(connection, newPath, mode)
		case "file":
			readFile(connection, newPath, mode, size)
		case "exit":
			return
		}
	}

}

func to(tocreate string, connection ssh.Channel) {

	connection.Write([]byte{0})

	t, mode, size, filename := readProtocolControl(connection)
	log.Printf("%s %#o %d %s\n", t, mode, size, filename)

	switch t {
	case "dir":

	case "file":
		pathinfo, err := os.Stat(tocreate)
		if err != nil && !os.IsNotExist(err) {
			log.Println(err)
			return
		}

		var path string = tocreate
		if pathinfo != nil && pathinfo.IsDir() {
			path = filepath.Join(tocreate, filename)
		}

		log.Println(readFile(connection, path, mode, size))
	default:

	}

}

func from(todownload string, connection ssh.Channel) {
	clientReady, _ := readAck(connection)
	if clientReady != 0 {
		log.Println("Client failed to ready up")
		return
	}

	fileinfo, err := os.Stat(todownload)
	if err != nil {
		internal.ScpError(fmt.Sprintf("error: %s", err), connection)
		log.Println("File not found")
		return
	}

	if fileinfo.Mode().IsRegular() {
		log.Println("Transfering single file")
		log.Println(scpTransferFile(todownload, fileinfo, connection))
	}

	if fileinfo.IsDir() {
		log.Println("Transferring directory")
		log.Println(scpTransferDirectory(todownload, fileinfo, connection))
	}

	connection.Write([]byte("E\n"))
	success, _ := readAck(connection)
	if success != 0 {
		log.Println("Final end failed")
	}
}

func scpTransferDirectory(path string, mode fs.FileInfo, connection ssh.Channel) error {
	connection.Write([]byte(fmt.Sprintf("D%#o 1 %s\n", mode.Mode().Perm(), filepath.Base(path))))

	success, _ := readAck(connection)
	if success != 0 {
		return errors.New("Creating directory failed")
	}

	dir, err := os.Open(path)
	if err != nil {
		return err
	}

	files, err := dir.Readdir(-1)
	dir.Close()
	if err != nil {
		return err
	}

	for _, file := range files {
		if file.IsDir() {
			err := scpTransferDirectory(filepath.Join(path, file.Name()), file, connection)
			if err != nil {
				return err
			}
			continue
		}

		err := scpTransferFile(filepath.Join(path, file.Name()), file, connection)
		if err != nil {
			return err
		}
	}

	connection.Write([]byte("E\n"))

	return nil
}

func readAck(conn ssh.Channel) (int, error) {

	buf := make([]byte, 1)
	_, err := conn.Read(buf)
	if err != nil {
		return -1, err
	}

	return int(buf[0]), nil

}

func scpTransferFile(path string, fi fs.FileInfo, connection ssh.Channel) error {

	_, err := fmt.Fprintf(connection, "C%#o %d %s\n", fi.Mode(), fi.Size(), filepath.Base(path))
	if err != nil {
		return err
	}

	readyToAcceptFile, _ := readAck(connection)
	if readyToAcceptFile != 0 {
		return errors.New("Client unable to receive new file")
	}

	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	defer func() {
		connection.Write([]byte{0})
		failedToFinish, _ := readAck(connection)
		if failedToFinish != 0 {
			log.Println("Unable to finish up file")
		}
	}()

	buf := make([]byte, 1024)
	for {
		n, err := file.Read(buf)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}

		nn, err := connection.Write(buf[:n])
		if nn < n {
			return errors.New("Not able to do full write, connection is bad")
		}

		if err != nil {
			return err
		}

		if n < 1024 {
			return nil
		}
	}
}
