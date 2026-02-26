/**
 * Fibonacci 3D Generator - Fixed orientation & centering
 * Deploy: picoflare deploy-fib3d
 */
const HTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Fibonacci 3D Generator</title>
  <script src="https://cdnjs.cloudflare.com/ajax/libs/three.js/r128/three.min.js"></script>
  <script src="https://cdn.jsdelivr.net/npm/three@0.128.0/examples/js/controls/OrbitControls.js"></script>
  <style>
    * { margin: 0; padding: 0; box-sizing: border-box; }
    body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; background: linear-gradient(135deg, #1a1a2e 0%, #16213e 100%); min-height: 100vh; color: #fff; display: flex; flex-direction: column; align-items: center; padding: 1rem; }
    h1 { font-size: 2rem; margin-bottom: 0.5rem; background: linear-gradient(90deg, #ffd700, #ff6b6b); -webkit-background-clip: text; -webkit-text-fill-color: transparent; }
    .subtitle { color: #888; margin-bottom: 1rem; font-size: 0.9rem; }
    .container { background: rgba(255,255,255,0.05); border-radius: 20px; padding: 1.5rem; max-width: 500px; width: 100%; backdrop-filter: blur(10px); border: 1px solid rgba(255,255,255,0.1); }
    .control { margin-bottom: 1rem; }
    label { display: block; margin-bottom: 0.3rem; color: #ccc; font-size: 0.85rem; }
    input[type="range"] { width: 100%; height: 8px; border-radius: 4px; background: rgba(255,255,255,0.1); outline: none; -webkit-appearance: none; }
    input[type="range"]::-webkit-slider-thumb { -webkit-appearance: none; width: 18px; height: 18px; border-radius: 50%; background: linear-gradient(135deg, #ffd700, #ff6b6b); cursor: pointer; }
    .value { text-align: right; color: #ffd700; font-weight: bold; font-size: 0.85rem; }
    button { width: 100%; padding: 0.8rem; border: none; border-radius: 12px; background: linear-gradient(135deg, #ffd700, #ff6b6b); color: #1a1a2e; font-size: 1rem; font-weight: bold; cursor: pointer; margin-bottom: 0.5rem; transition: transform 0.2s, box-shadow 0.2s; }
    button:hover { transform: translateY(-2px); box-shadow: 0 10px 30px rgba(255, 215, 0, 0.3); }
    button:disabled { opacity: 0.6; cursor: not-allowed; transform: none; }
    .status { margin-top: 0.5rem; padding: 0.8rem; border-radius: 8px; text-align: center; display: none; font-size: 0.9rem; }
    .status.show { display: block; }
    .status.loading { background: rgba(255,215,0,0.1); color: #ffd700; }
    .status.success { background: rgba(0,255,0,0.1); color: #0f0; }
    .status.error { background: rgba(255,0,0,0.1); color: #f66; }
    .preview { margin-top: 1rem; text-align: center; background: #1a1a2e; border-radius: 12px; padding: 0.5rem; width: 100%; max-width: 600px; }
    #canvas-container { width: 100%; height: 400px; border-radius: 8px; overflow: hidden; }
    .preview-info { display: flex; justify-content: space-around; margin-top: 0.5rem; font-size: 0.75rem; color: #888; }
    .preview-info span { color: #ffd700; font-weight: bold; }
    footer { margin-top: 1.5rem; color: #666; font-size: 0.75rem; }
    @media (max-width: 600px) { #canvas-container { height: 300px; } h1 { font-size: 1.5rem; } }
  </style>
</head>
<body>
  <h1>Fibonacci 3D</h1>
  <p class="subtitle">Golden ratio spiral 3D models</p>
  <div class="container">
    <div class="control">
      <label>Spiral Iterations</label>
      <input type="range" id="iterations" min="3" max="12" value="8">
      <div class="value" id="iterationsValue">8</div>
    </div>
    <div class="control">
      <label>Model Scale</label>
      <input type="range" id="scale" min="5" max="50" value="15">
      <div class="value" id="scaleValue">15</div>
    </div>
    <button id="generateBtn">Generate 3D Model</button>
    <button id="downloadBtn" disabled style="display:none;">Download STL</button>
    <div class="status" id="status"></div>
  </div>
  <div class="preview">
    <div id="canvas-container"></div>
    <div class="preview-info">
      <div>Vertices: <span id="vcount">0</span></div>
      <div>Faces: <span id="fcount">0</span></div>
      <div>Size: <span id="fsize">0 KB</span></div>
    </div>
  </div>
  <footer>Powered by Cloudflare Workers • PicoFlare</footer>
  <script>
    let scene, camera, renderer, controls, mesh;
    let currentData = null;
    
    function init() {
      const container = document.getElementById('canvas-container');
      scene = new THREE.Scene();
      scene.background = new THREE.Color(0x1a1a2e);
      camera = new THREE.PerspectiveCamera(45, container.clientWidth / container.clientHeight, 0.1, 1000);
      camera.position.set(5, 5, 5);
      renderer = new THREE.WebGLRenderer({ antialias: true });
      renderer.setSize(container.clientWidth, container.clientHeight);
      renderer.setPixelRatio(window.devicePixelRatio);
      container.appendChild(renderer.domElement);
      controls = new THREE.OrbitControls(camera, renderer.domElement);
      controls.enableDamping = true;
      controls.dampingFactor = 0.05;
      controls.minDistance = 1;
      controls.maxDistance = 100;
      scene.add(new THREE.AmbientLight(0x404040, 1.5));
      scene.add(new THREE.DirectionalLight(0xffd700, 1).position.set(10, 10, 5));
      scene.add(new THREE.DirectionalLight(0xff6b6b, 0.5).position.set(-10, -5, -5));
      function animate() { requestAnimationFrame(animate); controls.update(); renderer.render(scene, camera); }
      animate();
      window.addEventListener('resize', () => { camera.aspect = container.clientWidth / container.clientHeight; camera.updateProjectionMatrix(); renderer.setSize(container.clientWidth, container.clientHeight); });
      generateModel();
    }
    
    function generateFibonacciMesh(iterations, scale) {
      const phi = 1.618033988749895;
      const points = [];
      for (let i = 0; i < iterations * 10; i++) {
        const t = i * 0.1;
        const r = scale * Math.pow(phi, t / Math.PI);
        points.push([r * Math.cos(t), r * Math.sin(t), scale * Math.sin(t * 0.5)]);
      }
      const vertices = [], indices = [];
      const segments = 8, thickness = scale * 0.15;
      for (let i = 0; i < points.length - 1; i++) {
        const [x1, y1, z1] = points[i], [x2, y2, z2] = points[i + 1];
        const r1 = thickness * (1 - i / points.length * 0.5), r2 = thickness * (1 - (i + 1) / points.length * 0.5);
        const dx = x2 - x1, dy = y2 - y1, dz = z2 - z1;
        const len = Math.sqrt(dx * dx + dy * dy + dz * dz);
        if (len < 0.001) continue;
        const px = -dy / len, py = dx / len;
        const startIdx = vertices.length / 3;
        for (let j = 0; j <= segments; j++) {
          const a = (j / segments) * Math.PI * 2;
          vertices.push(x1 + r1 * Math.cos(a) * px, y1 + r1 * Math.cos(a) * py, z1 + r1 * Math.sin(a));
          vertices.push(x2 + r2 * Math.cos(a) * px, y2 + r2 * Math.cos(a) * py, z2 + r2 * Math.sin(a));
        }
        for (let j = 0; j < segments; j++) {
          const a = j * 2, b = j * 2 + 1, c = (j + 1) * 2, d = (j + 1) * 2 + 1;
          indices.push(startIdx + a, startIdx + b, startIdx + c, startIdx + b, startIdx + d, startIdx + c);
        }
      }
      return { vertices, indices };
    }
    
    function generateModel() {
      const iterations = parseInt(document.getElementById('iterations').value);
      const scale = parseInt(document.getElementById('scale').value);
      const status = document.getElementById('status');
      const downloadBtn = document.getElementById('downloadBtn');
      status.className = 'status show loading';
      status.textContent = 'Generating...';
      setTimeout(() => {
        try {
          const meshData = generateFibonacciMesh(iterations, scale);
          currentData = meshData;
          if (mesh) { scene.remove(mesh); mesh.geometry.dispose(); }
          const geometry = new THREE.BufferGeometry();
          geometry.setAttribute('position', new THREE.Float32BufferAttribute(meshData.vertices, 3));
          geometry.setIndex(meshData.indices);
          geometry.computeVertexNormals();
          const material = new THREE.MeshStandardMaterial({ color: 0xffd700, metalness: 0.7, roughness: 0.3, emissive: 0xff6600, emissiveIntensity: 0.1 });
          mesh = new THREE.Mesh(geometry, material);
          mesh.rotation.y = Math.PI / 2;
          scene.add(mesh);
          const box = new THREE.Box3().setFromObject(mesh);
          const center = new THREE.Vector3();
          box.getCenter(center);
          mesh.position.sub(center);
          const size = box.getSize(new THREE.Vector3());
          const maxDim = Math.max(size.x, size.y, size.z);
          const distance = maxDim * 2;
          camera.position.set(distance, distance, distance);
          camera.lookAt(0, 0, 0);
          controls.target.set(0, 0, 0);
          controls.update();
          document.getElementById('vcount').textContent = Math.floor(meshData.vertices.length / 3).toLocaleString();
          document.getElementById('fcount').textContent = Math.floor(meshData.indices.length / 3).toLocaleString();
          document.getElementById('fsize').textContent = Math.round((80 + 4 + (meshData.indices.length / 3) * 50) / 1024) + ' KB';
          downloadBtn.style.display = 'block';
          downloadBtn.disabled = false;
          status.className = 'status show success';
          status.textContent = 'Model ready!';
          setTimeout(() => status.classList.remove('show'), 2000);
        } catch (e) { status.className = 'status show error'; status.textContent = 'Error: ' + e.message; }
      }, 50);
    }
    
    function downloadSTL() {
      if (!currentData) return;
      const { vertices, indices } = currentData;
      let stl = 'solid FibonacciSpiral\\n';
      for (let i = 0; i < indices.length; i += 3) {
        const i0 = indices[i] * 3, i1 = indices[i + 1] * 3, i2 = indices[i + 2] * 3;
        const v0 = new THREE.Vector3(vertices[i0], vertices[i0 + 1], vertices[i0 + 2]);
        const v1 = new THREE.Vector3(vertices[i1], vertices[i1 + 1], vertices[i1 + 2]);
        const v2 = new THREE.Vector3(vertices[i2], vertices[i2 + 1], vertices[i2 + 2]);
        const n = new THREE.Vector3().crossVectors(v1.clone().sub(v0), v2.clone().sub(v0)).normalize();
        stl += '  facet normal ' + n.x.toExponential(6) + ' ' + n.y.toExponential(6) + ' ' + n.z.toExponential(6) + '\\n';
        stl += '    outer loop\\n';
        stl += '      vertex ' + v0.x.toExponential(6) + ' ' + v0.y.toExponential(6) + ' ' + v0.z.toExponential(6) + '\\n';
        stl += '      vertex ' + v1.x.toExponential(6) + ' ' + v1.y.toExponential(6) + ' ' + v1.z.toExponential(6) + '\\n';
        stl += '      vertex ' + v2.x.toExponential(6) + ' ' + v2.y.toExponential(6) + ' ' + v2.z.toExponential(6) + '\\n';
        stl += '    endloop\\n  endfacet\\n';
      }
      stl += 'endsolid FibonacciSpiral';
      const blob = new Blob([stl], { type: 'model/stl' });
      const a = document.createElement('a');
      a.href = URL.createObjectURL(blob);
      a.download = 'fibonacci.stl';
      a.click();
      URL.revokeObjectURL(a.href);
      document.getElementById('status').className = 'status show success';
      document.getElementById('status').textContent = 'STL downloaded!';
      setTimeout(() => document.getElementById('status').classList.remove('show'), 2000);
    }
    
    document.getElementById('iterations').oninput = function() { document.getElementById('iterationsValue').textContent = this.value; };
    document.getElementById('scale').oninput = function() { document.getElementById('scaleValue').textContent = this.value; };
    document.getElementById('generateBtn').onclick = generateModel;
    document.getElementById('downloadBtn').onclick = downloadSTL;
    init();
  </script>
</body>
</html>`;

export default {
  async fetch(request) {
    const url = new URL(request.url);
    if (url.pathname === '/api/generate' && request.method === 'POST') {
      try {
        const { iterations = 8, scale = 15 } = await request.json();
        const phi = 1.618033988749895;
        const points = [];
        for (let i = 0; i < iterations * 10; i++) {
          const t = i * 0.1;
          const r = scale * Math.pow(phi, t / Math.PI);
          points.push({ x: r * Math.cos(t), y: r * Math.sin(t), z: scale * Math.sin(t * 0.5) });
        }
        let stl = 'solid FibonacciSpiral\n';
        const thickness = scale * 0.15;
        const segments = 8;
        for (let i = 0; i < points.length - 1; i++) {
          const p1 = points[i], p2 = points[i + 1];
          const dx = p2.x - p1.x, dy = p2.y - p1.y, dz = p2.z - p1.z;
          const len = Math.sqrt(dx * dx + dy * dy + dz * dz);
          if (len < 0.001) continue;
          const r1 = thickness * (1 - i / points.length * 0.5), r2 = thickness * (1 - (i + 1) / points.length * 0.5);
          const px = -dy / len, py = dx / len;
          for (let j = 0; j < segments; j++) {
            const a1 = (j / segments) * Math.PI * 2, a2 = ((j + 1) / segments) * Math.PI * 2;
            const v0 = [p1.x + r1 * Math.cos(a1) * px, p1.y + r1 * Math.cos(a1) * py, p1.z + r1 * Math.sin(a1)];
            const v1 = [p2.x + r2 * Math.cos(a1) * px, p2.y + r2 * Math.cos(a1) * py, p2.z + r2 * Math.sin(a1)];
            const v2 = [p1.x + r1 * Math.cos(a2) * px, p1.y + r1 * Math.cos(a2) * py, p1.z + r1 * Math.sin(a2)];
            const v3 = [p2.x + r2 * Math.cos(a2) * px, p2.y + r2 * Math.cos(a2) * py, p2.z + r2 * Math.sin(a2)];
            const nx = (v1[1] - v0[1]) * (v2[2] - v0[2]) - (v1[2] - v0[2]) * (v2[1] - v0[1]);
            const ny = (v1[2] - v0[2]) * (v2[0] - v0[0]) - (v1[0] - v0[0]) * (v2[2] - v0[2]);
            const nz = (v1[0] - v0[0]) * (v2[1] - v0[1]) - (v1[1] - v0[1]) * (v2[0] - v0[0]);
            const nlen = Math.sqrt(nx * nx + ny * ny + nz * nz) || 1;
            stl += `  facet normal ${(nx / nlen).toExponential(6)} ${(ny / nlen).toExponential(6)} ${(nz / nlen).toExponential(6)}\n`;
            stl += '    outer loop\n';
            stl += `      vertex ${v0[0].toExponential(6)} ${v0[1].toExponential(6)} ${v0[2].toExponential(6)}\n`;
            stl += `      vertex ${v1[0].toExponential(6)} ${v1[1].toExponential(6)} ${v1[2].toExponential(6)}\n`;
            stl += `      vertex ${v2[0].toExponential(6)} ${v2[1].toExponential(6)} ${v2[2].toExponential(6)}\n`;
            stl += '    endloop\n  endfacet\n';
            stl += `  facet normal ${(nx / nlen).toExponential(6)} ${(ny / nlen).toExponential(6)} ${(nz / nlen).toExponential(6)}\n`;
            stl += '    outer loop\n';
            stl += `      vertex ${v1[0].toExponential(6)} ${v1[1].toExponential(6)} ${v1[2].toExponential(6)}\n`;
            stl += `      vertex ${v3[0].toExponential(6)} ${v3[1].toExponential(6)} ${v3[2].toExponential(6)}\n`;
            stl += `      vertex ${v2[0].toExponential(6)} ${v2[1].toExponential(6)} ${v2[2].toExponential(6)}\n`;
            stl += '    endloop\n  endfacet\n';
          }
        }
        stl += 'endsolid FibonacciSpiral';
        return new Response(stl, { headers: { 'Content-Type': 'model/stl', 'Content-Disposition': 'attachment; filename="fibonacci.stl"' } });
      } catch (e) {
        return new Response(JSON.stringify({ error: e.message }), { status: 500, headers: { 'Content-Type': 'application/json' } });
      }
    }
    return new Response(HTML, { headers: { 'Content-Type': 'text/html; charset=utf-8' } });
  },
};
