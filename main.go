package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
)

/* ===== MODELO ===== */

type VM struct {
	Nombre string `json:"nombre"`
	Disco  string `json:"disco"`
}

/* ===== CONFIG ===== */

// 🔥 CAMBIA ESTA RUTA A TU CARPETA REAL
const baseDiscos = "C:\\Users\\DIEGO\\VirtualBox VMs\\"

/* ===== CORS ===== */

func enableCors(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
}

/* ===== EJECUTAR COMANDOS (IMPORTANTE) ===== */

func runCommand(args []string) error {
	cmd := exec.Command(args[0], args[1:]...)
	out, err := cmd.CombinedOutput()

	if err != nil {
		return fmt.Errorf("ERROR: %s\nSALIDA: %s", err, string(out))
	}

	fmt.Println(string(out))
	return nil
}

/* ===== CREAR VM ===== */

func crearVMHandler(w http.ResponseWriter, r *http.Request) {

	enableCors(w)

	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}

	if r.Method != "POST" {
		http.Error(w, "Método no permitido", 405)
		return
	}

	var vm VM
	err := json.NewDecoder(r.Body).Decode(&vm)
	if err != nil {
		http.Error(w, "JSON inválido", 400)
		return
	}

	// 🔥 RUTA COMPLETA DEL DISCO
	rutaDisco := baseDiscos + vm.Disco

	// ===== COMANDOS CORRECTOS =====

	comandos := [][]string{

		// Crear VM
		{"VBoxManage", "createvm", "--name", vm.Nombre, "--register"},

		// RAM + CPU + BOOT
		{"VBoxManage", "modifyvm", vm.Nombre,
			"--memory", "1024",
			"--cpus", "1",
			"--boot1", "disk"},

		// Controlador SATA
		{"VBoxManage", "storagectl", vm.Nombre,
			"--name", "SATA",
			"--add", "sata",
			"--controller", "IntelAhci"},

		// 🔥 ADJUNTAR DISCO (CLAVE)
		{"VBoxManage", "storageattach", vm.Nombre,
			"--storagectl", "SATA",
			"--port", "0",
			"--device", "0",
			"--type", "hdd",
			"--medium", rutaDisco},

		// Red
		{"VBoxManage", "modifyvm", vm.Nombre,
			""--nic1", "nat"},

		// Iniciar VM
		{"VBoxManage", "startvm", vm.Nombre,
			"--type", "headless"},
	}

	for _, cmd := range comandos {
		err := runCommand(cmd)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
	}

	w.Write([]byte("✅ VM creada correctamente con disco 🔥"))
}

/* ===== MAIN ===== */

func main() {

	// API
	http.HandleFunc("/crearVM", crearVMHandler)

	// FRONTEND
	fs := http.FileServer(http.Dir("./static"))
	http.Handle("/", fs)

	fmt.Println("Servidor en http://localhost:8081")
	http.ListenAndServe(":8081", nil)
}