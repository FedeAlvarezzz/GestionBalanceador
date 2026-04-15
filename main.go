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
	"strconv"
	"sync"
	"time"
	"net"

	"golang.org/x/crypto/ssh"
)

// ─── ESTRUCTURAS ─────────────────────────────────────────────────────────────

type Disco struct {
	Nombre string
	Ruta   string
}

type Maquina struct {
	Nombre   string
	IP       string
	Puerto   string
	Temporal bool
}

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

// ─── ESTADO GLOBAL ────────────────────────────────────────────────────────────

var discosGuardados []Disco
var vmsGuardadas []Maquina
var balanceadoresGuardados []Balanceador
var resultadoConsola string
var mtx sync.Mutex

// CPU por nombre de VM
var cpuPorVM = make(map[string]float64)

// AutoScaling
var autoScalingActivo = true
var umbralAlto = 70.0
var umbralBajo = 20.0
var frecuenciaMuestreo = 10 * time.Second

// Tiempo acumulado que cada VM lleva por encima del umbral alto
var tiempoCalienteVM = make(map[string]time.Duration)
var ultimaMedicion = make(map[string]time.Time)

// VM temporal asociada a cada balanceador (balID -> nombre VM temporal)
var vmTemporalPorBal = make(map[string]string)

// Cooldown por balanceador (balID -> momento del último escalado)
var ultimoEscaladoPorBal = make(map[string]time.Time)

var duracionUmbral = 60 * time.Second // tiempo sostenido sobre umbral para escalar arriba
const cooldownEscalado = 2 * time.Minute // tiempo mínimo entre escalados

const archivoEstado = "state.json"

// ─── CREDENCIALES ────────────────────────────────────────────────────────────

const sshUser = "diego"
const sshPassword = "123"

// ─── PERSISTENCIA ────────────────────────────────────────────────────────────

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

// ─── SSH ─────────────────────────────────────────────────────────────────────

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

// ─── CPU ─────────────────────────────────────────────────────────────────────

func obtenerCPU(ip, puerto string) (float64, error) {
	client, err := creaClienteSSH(ip, puerto)
	if err != nil {
		return 0, err
	}
	defer client.Close()

	read := func() (idle, total float64, err error) {
		out, err := ejecutarComandoSSH(client, "cat /proc/stat | head -n 1")
		if err != nil {
			return 0, 0, err
		}

		fields := strings.Fields(out)
		if len(fields) < 5 {
			return 0, 0, fmt.Errorf("formato /proc/stat inválido")
		}

		var vals []float64
		for _, f := range fields[1:] {
			v, _ := strconv.ParseFloat(f, 64)
			vals = append(vals, v)
		}

		idle = vals[3]
		for _, v := range vals {
			total += v
		}

		return idle, total, nil
	}

	idle1, total1, err := read()
	if err != nil {
		return 0, err
	}

	time.Sleep(1 * time.Second)

	idle2, total2, err := read()
	if err != nil {
		return 0, err
	}

	diffIdle := idle2 - idle1
	diffTotal := total2 - total1

	if diffTotal == 0 {
		return 0, nil
	}

	cpu := 100 * (diffTotal - diffIdle) / diffTotal
	return cpu, nil
}

func cpuHandler(w http.ResponseWriter, r *http.Request) {
	mtx.Lock()
	defer mtx.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(cpuPorVM)
}

// ─── AUTOSCALING ─────────────────────────────────────────────────────────────

func evaluarAutoScalingPorBal(bal Balanceador) {
	mtx.Lock()

	if !autoScalingActivo {
		mtx.Unlock()
		return
	}

	// Cooldown por balanceador
	if t, ok := ultimoEscaladoPorBal[bal.ID]; ok {
		if time.Since(t) < cooldownEscalado {
			mtx.Unlock()
			return
		}
	}

var servidoresBase []ServidorBackend

for _, s := range bal.Servidores {
	if !strings.HasPrefix(s.Nombre, "AutoVM-") {
		servidoresBase = append(servidoresBase, s)
	}
}

// 🔥 CALCULAR CARGA PROMEDIO DEL BALANCEADOR
totalCPU := 0.0
count := 0

for _, s := range servidoresBase {
	if cpu, ok := cpuPorVM[s.Nombre]; ok {
		totalCPU += cpu
		count++
	}
}

if count == 0 {
	mtx.Unlock()
	return
}

avgCPU := totalCPU / float64(count)

var tiempoTotal time.Duration

for _, s := range servidoresBase {
	if t, ok := tiempoCalienteVM[s.Nombre]; ok {
		tiempoTotal += t
	}
}

hayVMCaliente := avgCPU >= umbralAlto && tiempoTotal >= duracionUmbral

	vmTempExiste := vmTemporalPorBal[bal.ID] != ""

	// ─── ESCALAR ARRIBA ───
	if hayVMCaliente && !vmTempExiste {
		log.Printf("[%s] CPU alta sostenida → creando VM temporal\n", bal.Nombre)
		ultimoEscaladoPorBal[bal.ID] = time.Now()
		mtx.Unlock()
		balCopy := bal
		if len(servidoresBase) == 0 {
	mtx.Unlock()
	return
}
vmOrigen := servidoresBase[0].IP
	go crearVMTemporalParaBal(balCopy.ID, vmOrigen)
		return
	}

	// ─── ESCALAR ABAJO ───
	if vmTempExiste {
		todasFrias := true
		for _, s := range servidoresBase {
			if cpu, ok := cpuPorVM[s.Nombre]; ok {
				if cpu > umbralBajo {
	todasFrias = false
	break
}
			}
		}

		if todasFrias {
			log.Printf("[%s] CPU baja → eliminando VM temporal\n", bal.Nombre)
			ultimoEscaladoPorBal[bal.ID] = time.Now()
			vmTemp := vmTemporalPorBal[bal.ID]
			mtx.Unlock()
			go eliminarVMTemporalDeBal(bal, vmTemp)
			return
		}
	}

	mtx.Unlock()
}

func crearVMTemporalParaBal(balID string, vmOrigen string) {

	vboxManage := "C:\\Program Files\\Oracle\\VirtualBox\\VBoxManage.exe"

	nombre := fmt.Sprintf("AutoVM-%d", time.Now().Unix())

	log.Printf("Creando VM desde plantilla -> %s\n", nombre)

	// 1. CREAR VM NUEVA
	exec.Command(vboxManage,
		"createvm",
		"--name", nombre,
		"--ostype", "Debian_64",
		"--register",
	).Run()

	// 2. CONFIGURAR HARDWARE
	exec.Command(vboxManage,
		"modifyvm", nombre,
		"--memory", "1024",
		"--cpus", "1",
		"--nic1", "bridged",
		"--bridgeadapter1",
		 "Intel(R) Wi-Fi 6 AX201 160MHz",
	).Run()

	// 3. CONTROLADOR DISCO (VACÍO o plantilla base)
	exec.Command(vboxManage,
		"storagectl", nombre,
		"--name", "SATA",
		"--add", "sata",
		"--controller", "IntelAhci",
	).Run()

	// 4. AQUÍ DECIDES ESTRATEGIA:
	// Opción A: disco nuevo
	// Opción B: disco base compartido (multiattach)

	baseDisk := "C:\\Users\\DIEGO\\VirtualBox VMs\\Debian.vdi"

	exec.Command(vboxManage,
		"storageattach", nombre,
		"--storagectl", "SATA",
		"--port", "0",
		"--device", "0",
		"--type", "hdd",
		"--medium", baseDisk,
	).Run()

	// 5. INICIAR
	cmdStart := exec.Command(vboxManage,
		"startvm", nombre,
		"--type", "headless",
	)

	if err := cmdStart.Run(); err != nil {
		log.Println("Error iniciando VM temporal:", err)
		return
	}

	// 6. REGISTRAR EN BALANCEADOR
	mtx.Lock()
	for i, b := range balanceadoresGuardados {
		if b.ID == balID {
			balanceadoresGuardados[i].Servidores = append(b.Servidores, ServidorBackend{
				ID:     nombre,
				Nombre: nombre,
				IP: vmOrigen, // En este ejemplo asumimos que la nueva VM obtiene la misma IP (ej. usando un disco base con MAC fija)
				Puerto: "8000",
			})
			vmTemporalPorBal[balID] = nombre
			break
		}
	}
	mtx.Unlock()

	sincronizarHAProxyParaBal(balanceadoresGuardados[0])
}

func eliminarVMTemporalDeBal(bal Balanceador, vmNombre string) {
	log.Printf("Eliminando VM temporal: %s\n", vmNombre)

	vboxManage := "C:\\Program Files\\Oracle\\VirtualBox\\VBoxManage.exe"
	exec.Command(vboxManage, "controlvm", vmNombre, "poweroff").Run()
	time.Sleep(5 * time.Second)
	exec.Command(vboxManage, "unregistervm", vmNombre, "--delete").Run()

	mtx.Lock()

	// Quitar de vmsGuardadas
	var nuevasVMs []Maquina
	for _, v := range vmsGuardadas {
		if v.Nombre != vmNombre {
			nuevasVMs = append(nuevasVMs, v)
		}
	}
	vmsGuardadas = nuevasVMs

	// Quitar del balanceador
	for i, b := range balanceadoresGuardados {
		if b.ID == bal.ID {
			var nuevosServ []ServidorBackend
			for _, s := range b.Servidores {
				if s.Nombre != vmNombre {
					nuevosServ = append(nuevosServ, s)
				}
			}
			balanceadoresGuardados[i].Servidores = nuevosServ
			bal = balanceadoresGuardados[i]
			break
		}
	}

	delete(vmTemporalPorBal, bal.ID)
	delete(tiempoCalienteVM, vmNombre)
	delete(cpuPorVM, vmNombre)
	guardarEstado()
	mtx.Unlock()

	// Re-sincronizar HAProxy sin la VM temporal
	sincronizarHAProxyParaBal(bal)

	log.Printf("VM temporal %s eliminada y HAProxy actualizado\n", vmNombre)
}

// sincronizarHAProxyParaBal aplica la config de HAProxy programáticamente
// (usado por el autoscaling). El handler HTTP llama a esta misma lógica.
func sincronizarHAProxyParaBal(bal Balanceador) {
	client, err := creaClienteSSH(bal.IP, bal.PuertoSSH)
	if err != nil {
		log.Printf("Error SSH al balanceador %s: %v\n", bal.Nombre, err)
		return
	}
	defer client.Close()

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
	if s.IP == "" || s.Puerto == "" || s.Nombre == "" {
		continue
	}
	if net.ParseIP(s.IP) == nil {
	log.Printf("IP inválida ignorada: %s (%s)\n", s.Nombre, s.IP)
	continue
}
}

	subirArchivoSSH(client, []byte(cfg), "/home/"+sshUser+"/haproxy_appgo.cfg")
	out, err := ejecutarComandoSSH(client, fmt.Sprintf(
		"echo '%s' | sudo -S mv /home/%s/haproxy_appgo.cfg /etc/haproxy/haproxy.cfg && echo '%s' | sudo -S systemctl restart haproxy",
		sshPassword, sshUser, sshPassword,
	))
	if err != nil {
		log.Printf("Error re-sincronizando HAProxy [%s]: %v\n%s\n", bal.Nombre, err, out)
	} else {
		log.Printf("HAProxy re-sincronizado para %s (%d backends)\n", bal.Nombre, len(bal.Servidores))
	}
}

func toggleAutoScalingHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		mtx.Lock()
		autoScalingActivo = !autoScalingActivo
		estado := autoScalingActivo
		mtx.Unlock()
		log.Println("AUTO SCALING:", estado)
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ─── HANDLERS HTTP ────────────────────────────────────────────────────────────

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
	Discos        []Disco
	VMs           []MaquinaVirtualUI
	Balanceadores []Balanceador
	ResultadoServicio string
	CPU           map[string]float64

	AutoScaling   bool
	UmbralAlto    float64
	UmbralBajo    float64
	Frecuencia    time.Duration
}{
	Discos: discosGuardados,
	VMs: vmsUI,
	Balanceadores: balanceadoresGuardados,
	ResultadoServicio: resultadoConsola,
	CPU: cpuPorVM,

	AutoScaling: autoScalingActivo,
	UmbralAlto: umbralAlto,
	UmbralBajo: umbralBajo,
	Frecuencia: frecuenciaMuestreo,
}
	resultadoConsola = ""
	mtx.Unlock()
	tmpl.Execute(w, datos)
}

func ejecutarHandler(w http.ResponseWriter, r *http.Request) {
	r.ParseMultipartForm(50 << 20)

	puerto := r.FormValue("puerto")
	discoMulti := r.FormValue("disco_multi")
	ipVM := r.FormValue("ip_vm")
	vmName := r.FormValue("vm_plantilla")

	if vmName == "" || ipVM == "" || puerto == "" {
		fmt.Println("ERROR: Datos incompletos")
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	fmt.Println("Desplegando en:", vmName)

	client, err := creaClienteSSH(ipVM, "22")
	if err != nil {
		fmt.Println("Error SSH:", err)
		return
	}

	fileEjecutable, header, err := r.FormFile("ejecutable")
	if err != nil {
		fmt.Println("No se subio ejecutable:", err)
		return
	}
	defer fileEjecutable.Close()

	bytesEjecutable, err := io.ReadAll(fileEjecutable)
	if err != nil || len(bytesEjecutable) == 0 {
		fmt.Println("Ejecutable vacio")
		return
	}

	nombreEjecutable := strings.TrimSpace(header.Filename)
	fmt.Println("Ejecutable:", nombreEjecutable)

	err = subirArchivoSSH(client, bytesEjecutable, "/home/"+sshUser+"/"+nombreEjecutable)
	if err != nil {
		fmt.Println("Error subiendo ejecutable:", err)
		return
	}

	fileZip, _, err := r.FormFile("archivos_zip")
	if err == nil {
		defer fileZip.Close()
		bytesZip, _ := io.ReadAll(fileZip)
		subirArchivoSSH(client, bytesZip, "/home/"+sshUser+"/archivos.zip")
	}

	serviceData := fmt.Sprintf(`[Unit]
Description=Servidor Gestionado Go

[Service]
ExecStart=/bin/bash -c '/home/%s/%s %s'
WorkingDirectory=/home/%s/
Restart=always
User=%s

[Install]
WantedBy=multi-user.target`,
		sshUser, nombreEjecutable, puerto, sshUser, sshUser,
	)

	subirArchivoSSH(client, []byte(serviceData), "/home/"+sshUser+"/appweb.service")

	comandos := fmt.Sprintf(`
set -e
cd /home/%s
echo '%s' | sudo -S apt-get update -y
echo '%s' | sudo -S apt-get install -y unzip
if [ -f archivos.zip ]; then
	unzip -o archivos.zip
fi
chmod +x %s
echo "=== DEBUG ARCHIVOS ==="
ls -l /home/%s/
echo '%s' | sudo -S cp /home/%s/appweb.service /etc/systemd/system/appweb.service
echo '%s' | sudo -S systemctl daemon-reload
echo '%s' | sudo -S systemctl enable appweb
echo '%s' | sudo -S systemctl restart appweb
echo "=== STATUS ==="
echo '%s' | sudo -S systemctl status appweb --no-pager
`,
		sshUser,
		sshPassword, sshPassword,
		nombreEjecutable,
		sshUser,
		sshPassword, sshUser,
		sshPassword, sshPassword, sshPassword,
		sshPassword,
	)

	out2, err := ejecutarComandoSSH(client, comandos)
	fmt.Println("====== RESULTADO ======")
	fmt.Println(out2)
	if err != nil {
		fmt.Println("ERROR FINAL:", err)
	}
	client.Close()

	vboxManage := "C:\\Program Files\\Oracle\\VirtualBox\\VBoxManage.exe"
	cmd := exec.Command(vboxManage, "showvminfo", vmName, "--machinereadable")
	output, _ := cmd.Output()
	lines := strings.Split(string(output), "\n")
	vdiPath := ""
	for _, line := range lines {
		if strings.Contains(line, ".vdi") && strings.Contains(line, "SATA") {
			parts := strings.Split(line, "=")
			if len(parts) == 2 {
				vdiPath = strings.Trim(parts[1], `"`)
				break
			}
		}
	}

	if vdiPath != "" {
		mtx.Lock()
		discosGuardados = append(discosGuardados, Disco{
			Nombre: discoMulti,
			Ruta:   vdiPath,
		})
		guardarEstado()
		mtx.Unlock()
	}

	fmt.Println("Despliegue terminado")
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
	exec.Command(vboxManage, "modifyvm", vmNombre, "--memory", "1024", "--nic1", "bridged", "--bridgeadapter1", "Intel(R) Wi-Fi 6 AX201 160MHz").Run()
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

		comando := fmt.Sprintf("echo '%s' | sudo -S systemctl %s appweb --no-pager", sshPassword, accion)
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

		mtx.Lock()
		balanceadoresGuardados = append(balanceadoresGuardados, Balanceador{
			ID:        nombre,
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
			sincronizarHAProxyParaBal(*bal)

			mtx.Lock()
			resultadoConsola = fmt.Sprintf("HAProxy '%s' sincronizado exitosamente.\nEscuchando en Web Port: %s\nTarget Servers: %d",
				bal.Nombre, bal.PuertoWeb, len(bal.Servidores))
			mtx.Unlock()
		}
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func configAutoScalingHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	r.ParseForm()

	mtx.Lock()
	defer mtx.Unlock()

	if v := r.FormValue("umbral_alto"); v != "" {
		if val, err := strconv.ParseFloat(v, 64); err == nil {
			umbralAlto = val
		}
	}

	if v := r.FormValue("umbral_bajo"); v != "" {
		if val, err := strconv.ParseFloat(v, 64); err == nil {
			umbralBajo = val
		}
	}

if v := r.FormValue("tiempo_umbral"); v != "" {
	if val, err := strconv.Atoi(v); err == nil {
		duracionUmbral = time.Duration(val) * time.Second
	}
}

	if v := r.FormValue("frecuencia"); v != "" {
		if val, err := strconv.Atoi(v); err == nil {
			frecuenciaMuestreo = time.Duration(val) * time.Second
		}
	}

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ─── MAIN ─────────────────────────────────────────────────────────────────────

func main() {
	cargarEstado()

	// Monitor de CPU y autoscaling
	go func() {
		log.Println("MONITOR CPU INICIADO")

		for {
			mtx.Lock()
			vms := append([]Maquina(nil), vmsGuardadas...)
			bals := append([]Balanceador(nil), balanceadoresGuardados...)
			mtx.Unlock()

			ahora := time.Now()

			for _, vm := range vms {
				cpu, err := obtenerCPU(vm.IP, "22")
				if err != nil {
					log.Printf("Error CPU [%s - %s]: %v\n", vm.Nombre, vm.IP, err)
					continue
				}

				mtx.Lock()
				cpuPorVM[vm.Nombre] = cpu

				// Calcular delta real desde la última medición
				delta := 10 * time.Second
				if t, ok := ultimaMedicion[vm.Nombre]; ok {
					delta = ahora.Sub(t)
				}
				ultimaMedicion[vm.Nombre] = ahora

				// Acumular o resetear tiempo caliente
				if cpu > umbralAlto {
					tiempoCalienteVM[vm.Nombre] += delta
				} else {
					tiempoCalienteVM[vm.Nombre] = 0
				}

				caliente := tiempoCalienteVM[vm.Nombre]
				mtx.Unlock()

				log.Printf("CPU [%s - %s]: %.2f%% | tiempo caliente: %s\n",
					vm.Nombre, vm.IP, cpu, caliente.Round(time.Second))
			}

			// Evaluar autoscaling por cada balanceador
			for _, bal := range bals {
				evaluarAutoScalingPorBal(bal)
			}

			time.Sleep(frecuenciaMuestreo)
		}
	}()

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

	http.HandleFunc("/cpu", cpuHandler)
	http.HandleFunc("/autoscaling/toggle", toggleAutoScalingHandler)
	http.HandleFunc("/autoscaling/config", configAutoScalingHandler)

	fmt.Println("Servidor en ejecucion en http://localhost:8081")
	log.Fatal(http.ListenAndServe(":8081", nil))
}