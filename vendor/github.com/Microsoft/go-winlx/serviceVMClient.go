package winlx

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path"

	"strings"

	"io/ioutil"

	"github.com/Microsoft/hvsock"
)

func init() {
	// Get ID for hvsock. For now, assume that it is always up.
	// So, ignore the err for now.
	cmd := fmt.Sprintf("$(Get-ComputeProcess %s).Id", ServiceVMName)
	result, _ := exec.Command("powershell", cmd).Output()
	ServiceVMId, _ = hvsock.GUIDFromString(strings.TrimSpace(string(result)))
}

func connectToServer() (hvsock.Conn, error) {
	hvAddr := hvsock.HypervAddr{VMID: ServiceVMId, ServiceID: ServiceVMSocketId}
	return hvsock.Dial(hvAddr)
}

func sendLayer(c hvsock.Conn, hdr []byte, r io.Reader) error {
	// First send the header, then the payload, then EOF
	_, err := c.Write(hdr)
	if err != nil {
		return err
	}

	_, err = io.Copy(c, r)
	if err != nil {
		return err
	}

	return c.CloseWrite()
}

func waitForResponse(c net.Conn) (int64, error) {
	// Read header
	// TODO: Handle error case
	buf := [12]byte{}
	_, err := io.ReadFull(c, buf[:])
	if err != nil {
		return 0, err
	}

	if buf[0] != ResponseOKCmd {
		return 0, fmt.Errorf("Service VM failed")
	}
	return int64(binary.BigEndian.Uint64(buf[4:])), nil
}

func writeVHDFile(path string, r io.Reader) (int64, error) {
	fmt.Println(path)
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}

	// Ignore error since hvsocket can close and return a non eof after sending all the data.
	n, _ := io.Copy(f, r)
	return n, f.Close()
}

func ServiceVMImportLayer(layerPath string, reader io.Reader) (int64, error) {
	// Hv sockets don't support graceful/unidirectional shutdown, and the
	// hvsock wrapper works weirdly with the tar reader, so we first write the
	// contents to a temp file.
	tmpFile, err := ioutil.TempFile("", "docker-tar")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	fmt.Println("Copying tmpFile")
	_, err = io.Copy(tmpFile, reader)
	if err != nil {
		return 0, err
	}

	fmt.Println("Seeking to start of tmp file.")
	_, err = tmpFile.Seek(0, 0)
	if err != nil {
		return 0, err
	}

	fmt.Println("Connecting to server")
	conn, err := connectToServer()
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	header := ServiceVMHeader{
		Command:           ImportCmd,
		Version:           0,
		SCSIControllerNum: 0,
		SCSIDiskNum:       0,
	}

	buf, err := SerializeHeader(&header)
	if err != nil {
		return 0, err
	}

	fmt.Println("Sending message to service VM")
	err = sendLayer(conn, buf, tmpFile)
	if err != nil {
		return 0, err
	}

	fmt.Println("Waiting for server response")
	rSize, err := waitForResponse(conn)
	if err != nil {
		return 0, err
	}

	fmt.Println("writing vhd to file.")
	// We are getting the VHD stream, so write it to file
	fSize, err := writeVHDFile(path.Join(layerPath, LayerVHDName), conn)
	if err != nil {
		rSize = 0
	}

	if fSize != rSize {
		return 0, fmt.Errorf("hvsock closed before reading all data. Expected: %d, got: %d", rSize, fSize)
	}

	return rSize, err
}

func getVHDFile(vhdPath string) (*os.File, error) {
	// The vhd could be either layer.vhd or sandbox.vhdx depending on
	// if we are a ro layer or r/w layer

	vhdFile, err := os.Open(path.Join(vhdPath, "layer.vhd"))
	if err == nil {
		return vhdFile, err
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	// Try the sandbox path
	vhdFile, err = os.Open(path.Join(vhdPath, "sandbox.vhdx"))
	return vhdFile, err
}

func ServiceVMExportLayer(vhdPath string) (io.ReadCloser, error) {
	vhdFile, err := getVHDFile(vhdPath)
	if err != nil {
		return nil, err
	}
	defer vhdFile.Close()

	conn, err := connectToServer()
	if err != nil {
		return nil, err
	}

	header := ServiceVMHeader{
		Command:           ExportCmd,
		Version:           0,
		SCSIControllerNum: 0,
		SCSIDiskNum:       0,
	}

	buf, err := SerializeHeader(&header)
	if err != nil {
		conn.Close()
		return nil, err
	}

	fmt.Println("VHD PATH = ", vhdFile.Name())
	err = sendLayer(conn, buf, vhdFile)
	if err != nil {
		conn.Close()
		return nil, err
	}

	fmt.Println("Waiting for server response")
	_, err = waitForResponse(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}

	reader, writer := io.Pipe()
	go func() {
		defer writer.Close()
		defer conn.Close()
		fmt.Println("Copying result over hvsock")
		io.Copy(writer, conn)
	}()
	return reader, nil
}

func ServiceVMCreateSandbox(sandboxFolder string) error {
	// Right now just use powershell and bypass the service VM
	sandboxPath := path.Join(sandboxFolder, "sandbox.vhdx")
	fmt.Printf("ServiceVMCreateSandbox: Creating sandbox path: %s\n", sandboxPath)
	return exec.Command("powershell",
		"New-VHD",
		"-Path", sandboxPath,
		"-Dynamic",
		"-BlockSizeBytes", "1MB",
		"-SizeBytes", "16GB").Run()
}
