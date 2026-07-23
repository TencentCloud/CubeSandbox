<?php

declare(strict_types=1);

$path = parse_url($_SERVER['REQUEST_URI'] ?? '/', PHP_URL_PATH) ?: '/';

if ($path !== '/' && $path !== '/health' && $path !== '/api/hello' && $path !== '/api/state') {
    return false;
}

header('Content-Type: application/json; charset=utf-8');

function respond(int $status, array $payload): void
{
    http_response_code($status);
    echo json_encode($payload, JSON_THROW_ON_ERROR | JSON_UNESCAPED_SLASHES);
}

if ($path === '/' || $path === '/health') {
    respond(200, ['status' => 'ok', 'runtime' => 'php', 'workspace' => '/workspace']);
    return true;
}

if ($path === '/api/hello') {
    $name = trim((string) ($_GET['name'] ?? 'CubeSandbox'));
    respond(200, ['message' => "Hello, {$name}!", 'composer' => 'ready']);
    return true;
}

$statePath = '/workspace/state.json';
if ($_SERVER['REQUEST_METHOD'] === 'POST') {
    $input = json_decode((string) file_get_contents('php://input'), true);
    if (!is_array($input) || !is_string($input['message'] ?? null) || trim($input['message']) === '') {
        respond(400, ['error' => 'JSON body must contain a non-empty string field named message.']);
        return true;
    }

    $state = ['message' => $input['message'], 'updated_at' => gmdate(DATE_ATOM)];
    file_put_contents($statePath, json_encode($state, JSON_THROW_ON_ERROR) . "\n", LOCK_EX);
    respond(201, $state);
    return true;
}

if (is_file($statePath)) {
    $state = json_decode((string) file_get_contents($statePath), true, 512, JSON_THROW_ON_ERROR);
    respond(200, $state);
    return true;
}

respond(404, ['error' => 'No state has been saved yet. POST /api/state first.']);
return true;
