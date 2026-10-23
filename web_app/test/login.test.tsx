import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { client } from "../src/api";
import { Login } from "../src/pages/Login";

afterEach(cleanup);

describe("Login", () => {
  it("asks for the setup code on a fresh server", async () => {
    vi.spyOn(client, "serverInfo").mockResolvedValue({ name: "home", setup_required: true, read_only: false });
    render(<Login onSignedIn={() => {}} />);
    expect(await screen.findByText("Create admin account")).toBeTruthy();
    expect(screen.getByLabelText("Setup code")).toBeTruthy();
  });

  it("shows a normal sign-in otherwise", async () => {
    vi.spyOn(client, "serverInfo").mockResolvedValue({ name: "home", setup_required: false, read_only: true });
    render(<Login onSignedIn={() => {}} />);
    expect(await screen.findByText("Sign in")).toBeTruthy();
    expect(screen.queryByLabelText("Setup code")).toBeNull();
    expect(screen.getByText(/read-only/)).toBeTruthy();
  });
});
