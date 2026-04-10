const API = "http://localhost:8081";

document.getElementById("formVM").addEventListener("submit", async (e) => {
    e.preventDefault();

    const file = document.getElementById("discoVM").files[0];

    if (!file) {
        alert("Selecciona un disco");
        return;
    }

    const data = {
        nombre: document.getElementById("nombreVM").value,
        disco: file.name // 🔥 solo nombre
    };

    try {
        const res = await fetch(`${API}/crearVM`, {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify(data)
        });

        const text = await res.text();
        alert(text);

    } catch (err) {
        alert("Error creando VM");
        console.error(err);
    }
});