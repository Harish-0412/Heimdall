import React from "react";
import ReactDOM from "react-dom/client";
import App from "./App";
import Dashboard from "./dashboard/Dashboard";
import "./styles.css";
import "@fontsource-variable/dm-sans";
import "@fontsource-variable/manrope";

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    {window.location.pathname.replace(/\/$/, "") === "/dashboard" ? (
      <Dashboard />
    ) : (
      <App />
    )}
  </React.StrictMode>,
);
