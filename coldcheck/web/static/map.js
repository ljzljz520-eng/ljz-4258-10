let map, cellsSrc, layerNo = 0;

const emptyStyle = {
  version: 8,
  sources: {
    blank: { type: "geojson", data: { type: "FeatureCollection", features: [] } }
  },
  layers: [{ id: "bg", type: "background", paint: { "background-color": "#f4f6f8" } }]
};

function statusColor(p) {
  if (p.kind === "node") {
    if (p.status === "offline") return "#111";
  }
  if (p.kind === "zone") return "#dfe6ee";
  switch (p.status) {
    case "fail": return "#c62828";
    case "warn": return "#ef8a00";
    case "info": return "#f2c94c";
    default: return p.kind === "node" ? "#1565c0" : "#9fc88d";
  }
}

async function refresh() {
  const res = await fetch("/api/geojson");
  const fc = await res.json();
  const filtered = {
    type: "FeatureCollection",
    features: fc.features.filter(f => {
      const l = f.properties.layer;
      return l === layerNo;
    })
  };
  if (!map) init(filtered);
  else map.getSource("data").setData(filtered);
}

function init(data) {
  map = new maplibregl.Map({
    container: "map",
    style: emptyStyle,
    center: [6, 4],
    zoom: 8 // local-metre grid: zoomed close enough that 12m spans viewport
  });
  map.on("load", () => {
    map.addSource("data", { type: "geojson", data });
    map.addLayer({
      id: "zones", type: "fill", source: "data",
      filter: ["==", ["get", "kind"], "zone"],
      paint: { "fill-color": "#dfe6ee", "fill-opacity": 0.55 }
    });
    map.addLayer({
      id: "zone-line", type: "line", source: "data",
      filter: ["==", ["get", "kind"], "zone"],
      paint: { "line-color": "#7d8a99", "line-width": 2, "line-dasharray": [3, 2] }
    });
    map.addLayer({
      id: "cells", type: "fill", source: "data",
      filter: ["==", ["get", "kind"], "cell"],
      paint: {
        "fill-color": ["coalesce", ["feature-state", "color"],
          ["match", ["get", "status"],
            "fail", "#c62828", "warn", "#ef8a00", "info", "#f2c94c", "#9fc88d"]],
        "fill-opacity": 0.75
      }
    });
    map.addLayer({
      id: "cell-line", type: "line", source: "data",
      filter: ["==", ["get", "kind"], "cell"],
      paint: { "line-color": "#34495e", "line-width": 1 }
    });
    map.addLayer({
      id: "nodes", type: "circle", source: "data",
      filter: ["==", ["get", "kind"], "node"],
      paint: {
        "circle-radius": 7,
        "circle-color": ["match", ["get", "status"],
          "offline", "#111111", "fail", "#c62828", "warn", "#ef8a00", "#1565c0"],
        "circle-stroke-color": "#fff", "circle-stroke-width": 2
      }
    });

    function label(p) {
      let html = `<b>${p.code || p.id}</b>`;
      if (p.kind === "cell") {
        html += `<br>温区 ${p.zone}` +
          (p.nearDoor ? " · 近门口" : "") + (p.nearEvap ? " · 近蒸发器" : "") +
          (p.batches && p.batches.length ? `<br>在库批：${p.batches.join(", ")}` : "") +
          (p.layout ? `<br>库位版本：${p.layout}` : "") +
          `<br>状态：${p.status || "ok"}`;
      } else if (p.kind === "node") {
        html += `<br>空气节点 · ${p.cell} · ${p.status || "ok"}`;
      } else if (p.kind === "zone") {
        html += `<br>${p.name} 限值 [${p.min}, ${p.max}]°C` +
          (p.layout ? `<br>库位版本：${p.layout}` : "");
      }
      return html;
    }
    for (const kind of ["cells", "nodes", "zones"]) {
      map.on("click", kind, (e) => {
        const f = e.features[0];
        new maplibregl.Popup().setLngLat(e.lngLat).setHTML(label(f.properties)).addTo(map);
      });
    }
    map.fitBounds([[0, 0], [12, 8]], { padding: 30 });
  });
}

function setLayer(n) { layerNo = n; refresh(); }

refresh();
setInterval(refresh, 15000);
