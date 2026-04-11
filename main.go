package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

type Disco struct {
	Nombre string
	Ruta   string
}

type Maquina struct {
	Nombre string
	IP     string
	Puerto string
}

var discosGuardados []Disco
var vmsGuardadas []Maquina
var balanceadoresGuardados []Balanceador
var resultadoConsola string
var mtx sync.Mutex

type ServidorBackend struct {
	ID     string `json:"id"`
	Nombre string `json:"nombre"`
	IP     string `json:"ip"`
	Puerto string `json:"puerto"`
}

type Balanceador struct {
	ID         string            `json:"id"`
	Nombre     string            `json:"nombre"`
	IP         string            `json:"ip"`
	PuertoSSH  string            `json:"puerto_ssh"`
	PuertoWeb  string            `json:"puerto_web"`
	Servidores []ServidorBackend `json:"servidores"`
}

type EstadoApp struct {
	Discos        []Disco       `json:"discos"`
	VMs           []Maquina     `json:"vms"`
	Balanceadores []Balanceador `json:"balanceadores"`
}

type MaquinaVirtualUI struct {
	Nombre      string
	IP          string
	Puerto      string
	EsEncendida bool
}

const archivoEstado = "state.json"

// ─── CREDENCIALES ────────────────────────────────────────────────────────────
const sshUser = "diego"
const sshPassword = "123"

// ─────────────────────────────────────────────────────────────────────────────

func cargarEstado() {
	mtx.Lock()
	defer mtx.Unlock()
	data, err := os.ReadFile(archivoEstado)
	if err != nil {
		return
	}
	var estado EstadoApp
	if err := json.Unmarshal(data, &estado); err == nil {
		if estado.Discos != nil {
			discosGuardados = estado.Discos
		}
		if estado.VMs != nil {
			vmsGuardadas = estado.VMs
		}
		if estado.Balanceadores != nil {
			balanceadoresGuardados = estado.Balanceadores
		}
	}
}

func guardarEstado() {
	estado := EstadoApp{
		Discos:        discosGuardados,
		VMs:           vmsGuardadas,
		Balanceadores: balanceadoresGuardados,
	}
	data, err := json.MarshalIndent(estado, "", "  ")
	if err == nil {
		os.WriteFile(archivoEstado, data, 0644)
	}
}

// Funciones de conexion SSH
func creaClienteSSH(ip string, puerto string) (*ssh.Client, error) {
	var authMethods []ssh.AuthMethod

	// Intentar autenticacion con llave privada si existe
	// CAMBIAR: reemplaza TU_USUARIO con tu usuario de Windows
	key, err := os.ReadFile("C:\\Users\\TU_USUARIO\\.ssh\\id_ed25519")
	if err == nil {
		if signer, err := ssh.ParsePrivateKey(key); err == nil {
			authMethods = append(authMethods, ssh.PublicKeys(signer))
		}
	}

	// Autenticacion por contraseña
	authMethods = append(authMethods, ssh.Password(sshPassword))

	config := &ssh.ClientConfig{
		User:            sshUser,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}
	return ssh.Dial("tcp", ip+":"+puerto, config)
}

func subirArchivoSSH(client *ssh.Client, contenido []byte, rutaDestino string) error {
	session, _ := client.NewSession()
	defer session.Close()
	session.Stdin = bytes.NewReader(contenido)
	return session.Run("cat > " + rutaDestino)
}

func ejecutarComandoSSH(client *ssh.Client, comando string) (string, error) {
	session, _ := client.NewSession()
	defer session.Close()
	out, errCmd := session.CombinedOutput(comando)
	return string(out), errCmd
}

// Manejadores de rutas HTTP
func indexHandler(w http.ResponseWriter, r *http.Request) {
	tmpl, _ := template.ParseFiles("templates/index.html")

	vboxManage := "C:\\Program Files\\Oracle\\VirtualBox\\VBoxManage.exe"
	out, err := exec.Command(vboxManage, "list", "runningvms").Output()
	runningVMsOutput := ""
	if err == nil {
		runningVMsOutput = string(out)
	}

	mtx.Lock()
	var vmsUI []MaquinaVirtualUI
	for _, vm := range vmsGuardadas {
		encendida := strings.Contains(runningVMsOutput, fmt.Sprintf("\"%s\"", vm.Nombre))
		vmsUI = append(vmsUI, MaquinaVirtualUI{
			Nombre:      vm.Nombre,
			IP:          vm.IP,
			Puerto:      vm.Puerto,
			EsEncendida: encendida,
		})
	}

	datos := struct {
		Discos            []Disco
		VMs               []MaquinaVirtualUI
		Balanceadores     []Balanceador
		ResultadoServicio string
	}{
		Discos:            append([]Disco(nil), discosGuardados...),
		VMs:               vmsUI,
		Balanceadores:     append([]Balanceador(nil), balanceadoresGuardados...),
		ResultadoServicio: resultadoConsola,
	}
	resultadoConsola = ""
	mtx.Unlock()
	tmpl.Execute(w, datos)
}

func ejecutarHandler(w http.ResponseWriter, r *http.Request) {
	r.ParseMultipartForm(10 << 20)
	puerto := r.FormValue("puerto")
	discoMulti := r.FormValue("disco_multi")
	ipVM := r.FormValue("ip_vm")

	fmt.Println("--------------------------------------------------")
	fmt.Println("Iniciando despliegue en maquina base (Puerto 22)...")

	client, err := creaClienteSSH(ipVM, "22")
	if err != nil {
		fmt.Println("Error de conexion SSH:", err)
		return
	}

	// Inyeccion de llave publica
	// CAMBIAR: reemplaza TU_USUARIO con tu usuario de Windows
	pubKey, _ := os.ReadFile("C:\\Users\\TU_USUARIO\\.ssh\\id_ed25519.pub")
	ejecutarComandoSSH(client, fmt.Sprintf(`mkdir -p ~/.ssh && echo "%s" >> ~/.ssh/authorized_keys && chmod 700 ~/.ssh && chmod 600 ~/.ssh/authorized_keys`, string(pubKey)))
	fmt.Println("Llave SSH configurada exitosamente.")

	fmt.Println("Transfiriendo archivos...")
	fileEjecutable, _, _ := r.FormFile("ejecutable")
	bytesEjecutable, _ := io.ReadAll(fileEjecutable)
	subirArchivoSSH(client, bytesEjecutable, "/home/"+sshUser+"/ejecutable_linux")

	fileZip, _, _ := r.FormFile("archivos_zip")
	bytesZip, _ := io.ReadAll(fileZip)
	subirArchivoSSH(client, bytesZip, "/home/"+sshUser+"/archivos.zip")

	serviceData := fmt.Sprintf(`[Unit]
Description=Servidor Gestionado Go

[Service]
ExecStart=/home/%s/ejecutable_linux %s
WorkingDirectory=/home/%s/
Restart=always

[Install]
WantedBy=multi-user.target`, sshUser, puerto, sshUser)
	subirArchivoSSH(client, []byte(serviceData), "/home/"+sshUser+"/appweb.service")

	fmt.Println("Configurando sistema operativo y servicios...")
	comandosLinux := fmt.Sprintf(`
		set -e
		echo '%s' | sudo -S apt-get update -y
		echo '%s' | sudo -S apt-get install -y unzip
		unzip -o archivos.zip
		chmod +x ejecutable_linux
		echo '%s' | sudo -S cp /home/%s/appweb.service /etc/systemd/system/
		echo '%s' | sudo -S bash -c 'echo "%s ALL=(ALL) NOPASSWD: ALL" > /etc/sudoers.d/%s && chmod 440 /etc/sudoers.d/%s'
		echo '%s' | sudo -S systemctl daemon-reload
		echo '%s' | sudo -S systemctl enable appweb
		echo '%s' | sudo -S sync
	`, sshPassword, sshPassword, sshPassword, sshUser,
		sshPassword, sshUser, sshUser, sshUser,
		sshPassword, sshPassword, sshPassword)
	ejecutarComandoSSH(client, comandosLinux)

	outVerify, _ := ejecutarComandoSSH(client, "systemctl status appweb --no-pager")
	fmt.Println("Estado del servicio en la maquina base:\n", outVerify)

	client.Close()

	fmt.Println("Apagando maquina base...")
	// CAMBIAR: ajusta el nombre de la VM y la ruta del disco .vdi
	vboxManage := "C:\\Program Files\\Oracle\\VirtualBox\\VBoxManage.exe"
	vmName := "Debian-Servidor1"
	vdiPath := "C:\\Users\\TU_USUARIO\\VirtualBox VMs\\Debian-Servidor1\\Debian-Servidor.vdi"

	exec.Command(vboxManage, "controlvm", vmName, "poweroff").Run()
	time.Sleep(5 * time.Second)

	fmt.Println("Modificando medio de almacenamiento a multiconexion...")
	exec.Command(vboxManage, "storageattach", vmName, "--storagectl", "SATA", "--port", "0", "--device", "0", "--medium", "none").Run()
	exec.Command(vboxManage, "modifymedium", vdiPath, "--type", "multiattach").Run()

	mtx.Lock()
	discosGuardados = append(discosGuardados, Disco{Nombre: discoMulti, Ruta: vdiPath})
	guardarEstado()
	mtx.Unlock()

	fmt.Println("Despliegue finalizado exitosamente.")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func crearVMHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	r.ParseForm()
	discoRuta := r.FormValue("disco_ruta")
	vmNombre := r.FormValue("vm_nombre")
	ipVM := r.FormValue("ip_vm")

	vboxManage := "C:\\Program Files\\Oracle\\VirtualBox\\VBoxManage.exe"

	exec.Command(vboxManage, "createvm", "--name", vmNombre, "--ostype", "Debian_64", "--register").Run()
	exec.Command(vboxManage, "modifyvm", vmNombre, "--memory", "1024", "--nic1", "bridged", "--bridgeadapter1", "Ethernet").Run()
	exec.Command(vboxManage, "storagectl", vmNombre, "--name", "SATA", "--add", "sata", "--controller", "IntelAhci").Run()
	exec.Command(vboxManage, "storageattach", vmNombre, "--storagectl", "SATA", "--port", "0", "--device", "0", "--type", "hdd", "--medium", discoRuta).Run()
	exec.Command(vboxManage, "startvm", vmNombre, "--type", "headless").Run()

	mtx.Lock()
	vmsGuardadas = append(vmsGuardadas, Maquina{
		Nombre: vmNombre,
		IP:     ipVM,
		Puerto: "8000",
	})
	guardarEstado()
	mtx.Unlock()

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func servicioHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		r.ParseForm()
		puertoSSH := r.FormValue("puerto_ssh")
		accion := r.FormValue("accion")
		ipVM := r.FormValue("ip_vm")

		fmt.Println("--------------------------------------------------")
		fmt.Printf("Panel de control: Ejecutando accion '%s' en %s:%s...\n", accion, ipVM, puertoSSH)

		comando := fmt.Sprintf("sudo systemctl %s appweb --no-pager", accion)
		if accion == "logs" {
			comando = "sudo journalctl -u appweb -n 15 --no-pager"
		}

		client, err := creaClienteSSH(ipVM, puertoSSH)
		if err != nil {
			resultadoConsola = "Error de conexion SSH: " + err.Error()
		} else {
			defer client.Close()
			out, errCmd := ejecutarComandoSSH(client, comando)
			if errCmd != nil {
				resultadoConsola = fmt.Sprintf("Error ejecutando comando: %v\n\nSalida:\n%s", errCmd, string(out))
			} else {
				resultadoConsola = out
				if resultadoConsola == "" {
					resultadoConsola = "Comando ejecutado exitosamente (Sin salida estandar)."
				}
			}
		}
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func iniciarVMHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		vmNombre := r.FormValue("vm_nombre")
		fmt.Printf("Iniciando VM: %s\n", vmNombre)
		vboxManage := "C:\\Program Files\\Oracle\\VirtualBox\\VBoxManage.exe"
		exec.Command(vboxManage, "startvm", vmNombre, "--type", "headless").Run()
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func apagarVMHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		vmNombre := r.FormValue("vm_nombre")
		fmt.Printf("Apagando VM: %s\n", vmNombre)
		vboxManage := "C:\\Program Files\\Oracle\\VirtualBox\\VBoxManage.exe"
		exec.Command(vboxManage, "controlvm", vmNombre, "poweroff").Run()
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func eliminarDiscoHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		discoRuta := r.FormValue("disco_ruta")
		mtx.Lock()
		nuevosDiscos := []Disco{}
		for _, d := range discosGuardados {
			if d.Ruta != discoRuta {
				nuevosDiscos = append(nuevosDiscos, d)
			}
		}
		discosGuardados = nuevosDiscos
		guardarEstado()
		mtx.Unlock()
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func anadirBalanceadorHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		nombre := r.FormValue("nombre")
		ip := r.FormValue("ip")
		puertoSSH := r.FormValue("puerto_ssh")
		puertoWeb := r.FormValue("puerto_web")
		id := nombre

		mtx.Lock()
		balanceadoresGuardados = append(balanceadoresGuardados, Balanceador{
			ID:        id,
			Nombre:    nombre,
			IP:        ip,
			PuertoSSH: puertoSSH,
			PuertoWeb: puertoWeb,
		})
		guardarEstado()
		mtx.Unlock()
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func eliminarBalanceadorHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		id := r.FormValue("id")
		mtx.Lock()
		nuevos := []Balanceador{}
		for _, b := range balanceadoresGuardados {
			if b.ID != id {
				nuevos = append(nuevos, b)
			}
		}
		balanceadoresGuardados = nuevos
		guardarEstado()
		mtx.Unlock()
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func anadirServerBackendHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		balID := r.FormValue("bal_id")
		nombre := r.FormValue("nombre")
		ip := r.FormValue("ip")
		puerto := r.FormValue("puerto")

		mtx.Lock()
		for i, b := range balanceadoresGuardados {
			if b.ID == balID {
				balanceadoresGuardados[i].Servidores = append(b.Servidores, ServidorBackend{
					ID:     nombre,
					Nombre: nombre,
					IP:     ip,
					Puerto: puerto,
				})
				break
			}
		}
		guardarEstado()
		mtx.Unlock()
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func eliminarServerBackendHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		balID := r.FormValue("bal_id")
		serverID := r.FormValue("server_id")

		mtx.Lock()
		for i, b := range balanceadoresGuardados {
			if b.ID == balID {
				nuevos := []ServidorBackend{}
				for _, s := range b.Servidores {
					if s.ID != serverID {
						nuevos = append(nuevos, s)
					}
				}
				balanceadoresGuardados[i].Servidores = nuevos
				break
			}
		}
		guardarEstado()
		mtx.Unlock()
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func sincronizarHAProxyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		balID := r.FormValue("bal_id")

		mtx.Lock()
		var bal *Balanceador
		for i := range balanceadoresGuardados {
			if balanceadoresGuardados[i].ID == balID {
				bal = &balanceadoresGuardados[i]
				break
			}
		}
		mtx.Unlock()

		if bal != nil {
			fmt.Printf("Iniciando sincronizacion HAProxy para %s (IP: %s, Puerto SSH: %s)...\n", bal.Nombre, bal.IP, bal.PuertoSSH)

			client, err := creaClienteSSH(bal.IP, bal.PuertoSSH)
			if err != nil {
				mtx.Lock()
				resultadoConsola = "Error conectando SSH al balanceador: " + err.Error()
				mtx.Unlock()
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
			defer client.Close()

			fmt.Println("Verificando/Instalando HAProxy en VM...")
			ejecutarComandoSSH(client, fmt.Sprintf(
				"echo '%s' | sudo -S apt-get update && echo '%s' | sudo -S apt-get install -y haproxy",
				sshPassword, sshPassword,
			))

			cfg := `global
	log /dev/log	local0
	log /dev/log	local1 notice
	chroot /var/lib/haproxy
	stats socket /run/haproxy/admin.sock mode 660 level admin expose-fd listeners
	stats timeout 30s
	user haproxy
	group haproxy
	daemon

defaults
	log	global
	mode	http
	option	httplog
	option	dontlognull
	timeout connect 5000
	timeout client  50000
	timeout server  50000
	errorfile 400 /etc/haproxy/errors/400.http
	errorfile 500 /etc/haproxy/errors/500.http
	errorfile 502 /etc/haproxy/errors/502.http
	errorfile 503 /etc/haproxy/errors/503.http
	errorfile 504 /etc/haproxy/errors/504.http

frontend main_front
	bind *:` + bal.PuertoWeb + `
	default_backend main_back

backend main_back
	balance roundrobin
`
			for _, s := range bal.Servidores {
				cfg += fmt.Sprintf("\tserver %s %s:%s check\n", s.Nombre, s.IP, s.Puerto)
			}

			fmt.Println("Configurando balanceador...")
			subirArchivoSSH(client, []byte(cfg), "/home/"+sshUser+"/haproxy_appgo.cfg")
			out, errCmd := ejecutarComandoSSH(client, fmt.Sprintf(
				"echo '%s' | sudo -S mv /home/%s/haproxy_appgo.cfg /etc/haproxy/haproxy.cfg && echo '%s' | sudo -S systemctl restart haproxy",
				sshPassword, sshUser, sshPassword,
			))

			mtx.Lock()
			if errCmd != nil {
				resultadoConsola = fmt.Sprintf("Error configurando HAProxy: %v\nSalida: %s", errCmd, string(out))
			} else {
				resultadoConsola = fmt.Sprintf("HAProxy '%s' sincronizado exitosamente.\nEscuchando en Web Port: %s\nTarget Servers: %d", bal.Nombre, bal.PuertoWeb, len(bal.Servidores))
			}
			mtx.Unlock()
		}
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func main() {
	cargarEstado()

	http.HandleFunc("/", indexHandler)
	http.HandleFunc("/ejecutar", ejecutarHandler)
	http.HandleFunc("/crear-vm", crearVMHandler)
	http.HandleFunc("/servicio", servicioHandler)
	http.HandleFunc("/iniciar-vm", iniciarVMHandler)
	http.HandleFunc("/apagar-vm", apagarVMHandler)
	http.HandleFunc("/eliminar-disco", eliminarDiscoHandler)

	http.HandleFunc("/haproxy/nuevo", anadirBalanceadorHandler)
	http.HandleFunc("/haproxy/eliminar", eliminarBalanceadorHandler)
	http.HandleFunc("/haproxy/servidor/agregar", anadirServerBackendHandler)
	http.HandleFunc("/haproxy/servidor/eliminar", eliminarServerBackendHandler)
	http.HandleFunc("/haproxy/sincronizar", sincronizarHAProxyHandler)

	fmt.Println("Servidor en ejecucion en http://localhost:8081")
	log.Fatal(http.ListenAndServe(":8081", nil))
}