/*
 * JuiceFS, Copyright 2022 Juicedata, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package meta

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewClientWithErrorRejectsUnknownDriver(t *testing.T) {
	client, err := NewClientWithError("unknown-driver://metadata", nil)
	if client != nil {
		t.Fatal("client must be nil when the driver is unknown")
	}
	if err == nil || !strings.Contains(err.Error(), "invalid meta driver: unknown-driver") {
		t.Fatalf("expected invalid-driver error, got %v", err)
	}
}

func TestNewClientWithErrorReturnsCreatorFailure(t *testing.T) {
	const driver = "failing-driver-for-new-client-test"
	want := errors.New("backend unavailable")
	Register(driver, func(_, _ string, _ *Config) (Meta, error) {
		return nil, want
	})
	t.Cleanup(func() { delete(metaDrivers, driver) })

	client, err := NewClientWithError(driver+"://metadata", nil)
	if client != nil {
		t.Fatal("client must be nil when the creator fails")
	}
	if !errors.Is(err, want) {
		t.Fatalf("expected wrapped creator error, got %v", err)
	}
	if !strings.Contains(err.Error(), "meta "+driver+"://metadata is not available") {
		t.Fatalf("expected metadata address context, got %v", err)
	}
}

func TestNewClientWithErrorRedactsRedisParseAddress(t *testing.T) {
	const secret = "raw-secret"
	client, err := NewClientWithError("redis://user:"+secret+"%zz@localhost:6379/0", testConfig())
	if client != nil {
		t.Fatal("client must be nil when the Redis address is invalid")
	}
	if err == nil || !strings.Contains(err.Error(), "url parse redis://user:****@localhost:6379/0") {
		t.Fatalf("expected redacted Redis parse error, got %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("Redis parse error exposed metadata password: %v", err)
	}
}

func TestNewClientWithErrorSanitizesCreatorAddressAndPreservesCause(t *testing.T) {
	const driver = "address-repeating-driver-for-new-client-test"
	const secret = "raw-secret"
	want := errors.New("backend unavailable")
	Register(driver, func(_, addr string, _ *Config) (Meta, error) {
		return nil, fmt.Errorf("connect to %s: %w", addr, want)
	})
	t.Cleanup(func() { delete(metaDrivers, driver) })

	client, err := NewClientWithError(driver+"://user:"+secret+"@metadata", nil)
	if client != nil {
		t.Fatal("client must be nil when the creator fails")
	}
	if !errors.Is(err, want) {
		t.Fatalf("expected preserved creator error, got %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("creator error exposed metadata password: %v", err)
	}
}

func Test_injectPasswordIntoURI(t *testing.T) {
	const dbPasswd = "dbPasswd"
	tests := []struct {
		uri     string
		want    string
		wantErr bool
	}{
		//mysql
		{
			uri:     "mysql://root:password@(127.0.0.1:3306)/juicefs",
			want:    "mysql://root:password@(127.0.0.1:3306)/juicefs",
			wantErr: false,
		},
		{
			uri:     "mysql://root:@(127.0.0.1:3306)/juicefs",
			want:    "mysql://root:dbPasswd@(127.0.0.1:3306)/juicefs",
			wantErr: false,
		},
		{
			uri:     "mysql://root@(127.0.0.1:3306)/juicefs",
			want:    "mysql://root:dbPasswd@(127.0.0.1:3306)/juicefs",
			wantErr: false,
		},
		{
			uri:     "mysql://root@@(127.0.0.1:3306)/juicefs",
			want:    "mysql://root@:dbPasswd@(127.0.0.1:3306)/juicefs",
			wantErr: false,
		},
		// no user is ok
		{
			uri:     "mysql://:@(127.0.0.1:3306)/juicefs",
			want:    "mysql://:dbPasswd@(127.0.0.1:3306)/juicefs",
			wantErr: false,
		},
		{
			uri:     "mysql://:pwd@(127.0.0.1:3306)/juicefs",
			want:    "mysql://:pwd@(127.0.0.1:3306)/juicefs",
			wantErr: false,
		},
		//postgres
		{
			uri:     "postgres://root:password@192.168.1.6:5432/juicefs",
			want:    "postgres://root:password@192.168.1.6:5432/juicefs",
			wantErr: false,
		},
		{
			uri:     "postgres://root:@192.168.1.6:5432/juicefs",
			want:    "postgres://root:dbPasswd@192.168.1.6:5432/juicefs",
			wantErr: false,
		},
		{
			uri:     "postgres://root@192.168.1.6:5432/juicefs",
			want:    "postgres://root:dbPasswd@192.168.1.6:5432/juicefs",
			wantErr: false,
		},
		{
			uri:     "postgres://root@/pgtest?host=/tmp/pgsocket/&port=5433",
			want:    "postgres://root:dbPasswd@/pgtest?host=/tmp/pgsocket/&port=5433",
			wantErr: false,
		},
		{
			uri:     "postgres://@/pgtest?host=/tmp/pgsocket/&port=5433&user=pguser",
			want:    "postgres://:dbPasswd@/pgtest?host=/tmp/pgsocket/&port=5433&user=pguser",
			wantErr: false,
		},
		// Error conditions
		{
			uri:     "mysql://root(127.0.0.1:3306)/juicefs", // missing @
			want:    "",
			wantErr: true,
		},
		{
			uri:     "mysql://a:b:c:@(127.0.0.1:3306)/juicefs",
			want:    "",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			got, err := injectPasswordIntoURI(tt.uri, dbPasswd)

			if tt.wantErr {
				if err == nil {
					t.Errorf("injectPasswordIntoURI() expected error but got none")
					return
				}
			} else {
				if err != nil {
					t.Errorf("injectPasswordIntoURI() unexpected error = %v", err)
					return
				}
				if got != tt.want {
					t.Errorf("injectPasswordIntoURI() = %q, want %q", got, tt.want)
				}
			}
		})
	}
}

func Test_setPasswordFromEnv(t *testing.T) {
	tempDir := t.TempDir()
	passwordFile := filepath.Join(tempDir, "password.txt")
	err := os.WriteFile(passwordFile, []byte("filePassword"), 0600)
	if err != nil {
		t.Fatalf("Failed to create test password file: %v", err)
	}

	tests := []struct {
		name             string
		metaPassword     string
		metaPasswordFile string
		uri              string
		want             string
		wantErr          bool
	}{
		{
			name:         "META_PASSWORD only",
			metaPassword: "envPassword",
			uri:          "mysql://root@localhost/db",
			want:         "mysql://root:envPassword@localhost/db",
			wantErr:      false,
		},
		{
			name:             "META_PASSWORD_FILE only",
			metaPasswordFile: passwordFile,
			uri:              "mysql://root@localhost/db",
			want:             "mysql://root:filePassword@localhost/db",
			wantErr:          false,
		},
		{
			name:             "META_PASSWORD takes precedence over META_PASSWORD_FILE",
			metaPassword:     "envPassword",
			metaPasswordFile: passwordFile,
			uri:              "mysql://root@localhost/db",
			want:             "mysql://root:envPassword@localhost/db",
			wantErr:          false,
		},
		{
			name:    "neither environment variable set",
			uri:     "mysql://root@localhost/db",
			want:    "mysql://root@localhost/db",
			wantErr: false,
		},
		{
			name:             "META_PASSWORD_FILE points to non-existent file",
			metaPasswordFile: "/non/existent/file",
			uri:              "mysql://root@localhost/db",
			want:             "",
			wantErr:          true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clean environment
			defer os.Unsetenv("META_PASSWORD")
			defer os.Unsetenv("META_PASSWORD_FILE")
			// Just to be safe
			os.Unsetenv("META_PASSWORD")
			os.Unsetenv("META_PASSWORD_FILE")

			// Set environment variables as needed
			if tt.metaPassword != "" {
				os.Setenv("META_PASSWORD", tt.metaPassword)
			}
			if tt.metaPasswordFile != "" {
				os.Setenv("META_PASSWORD_FILE", tt.metaPasswordFile)
			}

			got, err := setPasswordFromEnv(tt.uri)

			if tt.wantErr {
				if err == nil {
					t.Errorf("setPasswordFromEnv() expected error but got none")
					return
				}
			} else {
				if err != nil {
					t.Errorf("setPasswordFromEnv() unexpected error = %v", err)
					return
				}
				if got != tt.want {
					t.Errorf("setPasswordFromEnv() = %q, want %q", got, tt.want)
				}
			}
		})
	}
}

func Test_readPasswordFromFile(t *testing.T) {
	// Create temporary directory for test files
	tempDir := t.TempDir()

	tests := []struct {
		name       string
		content    string
		filename   string
		createFile bool
		want       string
		wantErr    bool
	}{
		{
			name:       "valid password file",
			content:    "mypassword",
			filename:   "password.txt",
			createFile: true,
			want:       "mypassword",
			wantErr:    false,
		},
		{
			name:       "password with leading and trailing whitespace",
			content:    "\n  mypassword  \n\t",
			filename:   "password_with_spaces.txt",
			createFile: true,
			want:       "mypassword",
			wantErr:    false,
		},
		{
			name:       "empty file",
			content:    "",
			filename:   "empty.txt",
			createFile: true,
			want:       "",
			wantErr:    false,
		},
		{
			name:       "complex password with special characters",
			content:    "pa$$w0rd!@#",
			filename:   "complex.txt",
			createFile: true,
			want:       "pa$$w0rd!@#",
			wantErr:    false,
		},
		{
			name:       "file does not exist",
			content:    "",
			filename:   "nonexistent.txt",
			createFile: false,
			want:       "",
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var filePath string
			if tt.createFile {
				filePath = filepath.Join(tempDir, tt.filename)
				err := os.WriteFile(filePath, []byte(tt.content), 0600)
				if err != nil {
					t.Fatalf("Failed to create test file %s: %v", filePath, err)
				}
			} else {
				filePath = filepath.Join(tempDir, tt.filename)
			}

			got, err := readPasswordFromFile(filePath)

			if tt.wantErr {
				if err == nil {
					t.Errorf("readPasswordFromFile() expected error but got none")
					return
				}
			} else {
				if err != nil {
					t.Errorf("readPasswordFromFile() unexpected error = %v", err)
					return
				}
				if got != tt.want {
					t.Errorf("readPasswordFromFile() = %q, want %q", got, tt.want)
				}
			}
		})
	}
}
