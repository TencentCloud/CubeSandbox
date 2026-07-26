package com.example.cubesandbox;

import com.fasterxml.jackson.core.type.TypeReference;
import com.fasterxml.jackson.databind.ObjectMapper;
import java.io.IOException;
import java.nio.file.AtomicMoveNotSupportedException;
import java.nio.file.CopyOption;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.StandardCopyOption;
import java.time.Duration;
import java.time.Instant;
import java.time.format.DateTimeParseException;
import java.util.Iterator;
import java.util.LinkedHashMap;
import java.util.Map;
import java.util.UUID;
import java.util.concurrent.locks.ReentrantReadWriteLock;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.http.HttpStatus;
import org.springframework.stereotype.Component;
import org.springframework.web.server.ResponseStatusException;

@Component
public class TaskState {
    private static final int MAX_STORED_TASKS = 100;
    private static final int MAX_TITLE_LENGTH = 200;
    private static final int MAX_PAYLOAD_BYTES = 16 * 1024;
    private static final Duration TASK_TTL = Duration.ofHours(12);
    private static final String STATE_FILE_NAME = "tasks.json";
    private static final TypeReference<Map<String, Map<String, Object>>> TASKS_TYPE =
            new TypeReference<>() {};

    private final ObjectMapper objectMapper;
    private final Path stateDir;
    private final Path stateFile;
    private StateFileMover stateFileMover = Files::move;
    private final ReentrantReadWriteLock lock = new ReentrantReadWriteLock();

    public TaskState(
            ObjectMapper objectMapper,
            @Value("${cubesandbox.task-state-dir:/tmp/cubesandbox-spring/state}") String stateDir) {
        this.objectMapper = objectMapper;
        this.stateDir = Path.of(stateDir);
        this.stateFile = this.stateDir.resolve(STATE_FILE_NAME);
    }

    static TaskState forStateDir(ObjectMapper objectMapper, Path stateDir) {
        return new TaskState(objectMapper, stateDir.toString());
    }

    static TaskState forStateDir(ObjectMapper objectMapper, Path stateDir, StateFileMover stateFileMover) {
        TaskState taskState = forStateDir(objectMapper, stateDir);
        taskState.stateFileMover = stateFileMover;
        return taskState;
    }

    public Map<String, Object> createTask(CreateTaskRequest request) {
        String title = validatedTitle(request.title());
        Map<String, Object> payload = validatedPayload(request.payload());
        lock.writeLock().lock();
        try {
            Map<String, Map<String, Object>> tasks = readTasks();
            Instant now = Instant.now();
            pruneTasks(tasks, now);
            String id = UUID.randomUUID().toString();
            Map<String, Object> task = new LinkedHashMap<>();
            task.put("id", id);
            task.put("title", title);
            task.put("status", "created");
            task.put("createdAt", now.toString());
            task.put("payload", payload);
            task.put("persisted", true);
            tasks.put(id, task);
            writeTasks(tasks);
            return task;
        } finally {
            lock.writeLock().unlock();
        }
    }

    public Map<String, Object> getTask(String id) {
        lock.readLock().lock();
        try {
            Map<String, Object> task = readTasks().get(id);
            if (task == null || isExpired(task, Instant.now())) {
                throw new ResponseStatusException(HttpStatus.NOT_FOUND, "Task not found: " + id);
            }
            return task;
        } finally {
            lock.readLock().unlock();
        }
    }

    private String validatedTitle(String requestedTitle) {
        String title = requestedTitle == null || requestedTitle.isBlank() ? "demo task" : requestedTitle.strip();
        if (title.length() > MAX_TITLE_LENGTH) {
            throw new ResponseStatusException(
                    HttpStatus.BAD_REQUEST, "Task title must not exceed " + MAX_TITLE_LENGTH + " characters");
        }
        return title;
    }

    private Map<String, Object> validatedPayload(Map<String, Object> requestedPayload) {
        Map<String, Object> payload =
                requestedPayload == null ? Map.of() : new LinkedHashMap<>(requestedPayload);
        try {
            if (objectMapper.writeValueAsBytes(payload).length > MAX_PAYLOAD_BYTES) {
                throw new ResponseStatusException(
                        HttpStatus.BAD_REQUEST,
                        "Task payload must not exceed " + MAX_PAYLOAD_BYTES + " serialized bytes");
            }
        } catch (IOException e) {
            throw new ResponseStatusException(HttpStatus.BAD_REQUEST, "Task payload is not serializable", e);
        }
        return payload;
    }

    private void pruneTasks(Map<String, Map<String, Object>> tasks, Instant now) {
        tasks.entrySet().removeIf(entry -> isExpired(entry.getValue(), now));
        Iterator<String> ids = tasks.keySet().iterator();
        while (tasks.size() >= MAX_STORED_TASKS && ids.hasNext()) {
            ids.next();
            ids.remove();
        }
    }

    private boolean isExpired(Map<String, Object> task, Instant now) {
        Object createdAt = task.get("createdAt");
        if (!(createdAt instanceof String createdAtText)) {
            return true;
        }
        try {
            return Instant.parse(createdAtText).plus(TASK_TTL).isBefore(now);
        } catch (DateTimeParseException e) {
            return true;
        }
    }

    private Map<String, Map<String, Object>> readTasks() {
        if (!Files.exists(stateFile)) {
            return new LinkedHashMap<>();
        }
        try {
            return objectMapper.readValue(stateFile.toFile(), TASKS_TYPE);
        } catch (IOException e) {
            throw new ResponseStatusException(HttpStatus.INTERNAL_SERVER_ERROR, "Failed to read task state", e);
        }
    }

    private void writeTasks(Map<String, Map<String, Object>> tasks) {
        Path tempFile = null;
        ResponseStatusException failure = null;
        try {
            Files.createDirectories(stateDir);
            tempFile = Files.createTempFile(stateDir, "tasks", ".json.tmp");
            objectMapper.writerWithDefaultPrettyPrinter().writeValue(tempFile.toFile(), tasks);
            try {
                stateFileMover.move(
                        tempFile,
                        stateFile,
                        StandardCopyOption.ATOMIC_MOVE,
                        StandardCopyOption.REPLACE_EXISTING);
            } catch (AtomicMoveNotSupportedException e) {
                stateFileMover.move(tempFile, stateFile, StandardCopyOption.REPLACE_EXISTING);
            }
        } catch (IOException e) {
            failure =
                    new ResponseStatusException(HttpStatus.INTERNAL_SERVER_ERROR, "Failed to write task state", e);
            throw failure;
        } finally {
            if (tempFile != null) {
                try {
                    Files.deleteIfExists(tempFile);
                } catch (IOException cleanupError) {
                    if (failure != null) {
                        failure.addSuppressed(cleanupError);
                    } else {
                        throw new ResponseStatusException(
                                HttpStatus.INTERNAL_SERVER_ERROR,
                                "Failed to clean up temporary task state",
                                cleanupError);
                    }
                }
            }
        }
    }

    @FunctionalInterface
    interface StateFileMover {
        void move(Path source, Path target, CopyOption... options) throws IOException;
    }
}
