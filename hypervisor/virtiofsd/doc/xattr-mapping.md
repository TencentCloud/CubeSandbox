# Extended attribute (xattr) mapping

By default, the name of xattrs used by the client are passed through to the server
file system.  This can be a problem where either those xattr names are used
by something on the server (e.g. selinux client/server confusion) or if the
virtiofsd is running in a container with restricted privileges where it cannot
access some attributes.

## Mapping syntax

A mapping of xattr names can be made using `--xattrmap=<mapping>` where the `<mapping>`
string consists of a series of rules.

When looking for a mapping, the first matching rule applies.
There *must* be a mapping for every xattr name in the list of rules,
for example by making the final rule a catch-all rule to match any remaining attributes.

Each rule consists of a number of fields separated with a separator that is the
first non-white space character in the rule.  This separator must then be used
for the whole rule.
White space may be added before and after each rule.

Using `:` as the separator a rule is of the form:

```
:type:scope:key:prepend:
```

**scope** is one of:

- `client`: Match **key** against an xattr name from the client for
             setxattr/getxattr/removexattr
- `server`: Match **prepend** against an xattr name from the server
             for listxattr
- `all`: Can be used to make a single rule where both the server
          and client matches are triggered.

**type** is one of:

- `prefix`: Is designed to prepend and strip a prefix; the modified
  attributes then being passed on to the client/server.

- `ok`: Causes the rule set to be terminated when a match is found
  while allowing matching xattrs through unchanged.
  It is intended both as a way of explicitly terminating
  the list of rules, and to allow some xattrs to skip following rules.

- `bad`: If a client tries to use a name matching **key** it's
  denied using `EPERM`; when the server passes an attribute
  name matching **prepend** it's hidden.  In many ways its use is very like
  the `ok` type as either an explicit terminator or for special handling of certain
  patterns.

- `unsupported`: If a client tries to use a name matching **key** it's
  denied using `ENOTSUP`; when the server passes an attribute
  name matching **prepend** it's hidden.  In many ways its use is very like
  the `ok` type as either an explicit terminator or for special handling of certain
  patterns.

**key** is a string tested as a prefix on an attribute name originating
on the client.  It may be empty in which case a `client` scoped rule
will always match on client names.

**prepend** is a string tested as a prefix on an attribute name originating
on the server, and used as a new prefix. It may be empty
in which case a `server` scoped rule will always match on all names from
the server.

e.g.:

| Mapping rule                              | Description                                                                                          |
| ----------------------------------------- | ---------------------------------------------------------------------------------------------------- |
| `:prefix:client:trusted.:user.virtiofs.:` | will match `trusted.*` attributes in client calls and prefix them before passing them to the server. |
| `:prefix:server::user.virtiofs.:`         | will strip `user.virtiofs.` from all server replies.                                                 |
| `:prefix:all:trusted.:user.virtiofs.:`    | combines the previous two cases into a single rule.                                                  |
| `:ok:client:user.::`                      | will allow get/set xattr for `user.` xattrs.                                                         |
| `:ok:server::security.:`                  | will pass `security.` xattrs in listxattr from the server.                                           |
| `:ok:all:::`                              | will terminate the rule search passing any remaining attributes in both directions.                  |
| `:bad:server::security.:`                 | would hide `security.` xattrs in listxattr from the server.                                          |

A simpler **map** type provides a shorter syntax for the common case:

```
:map:key:prepend:
```

The `map` type adds a number of separate rules to add **prepend** as a prefix
to the matched **key** (or all attributes if **key** is empty).
There may be at most one `map` rule, and it must be the last rule in the set.

Please note that when the `security.capability` xattr is remapped, the daemon has to do
extra work to remove it during many operations, which the host kernel normally
does itself.

## Security considerations

Operating systems typically partition the xattr namespace using
well-defined name prefixes. Each partition may have different
access controls applied. For example, on Linux there are multiple
partitions

- `system.*`: access varies depending on attribute and filesystem
- `security.*`: only processes with `CAP_SYS_ADMIN`
- `trusted.*`: only processes with `CAP_SYS_ADMIN`
- `user.*`: any process granted by file permissions / ownership

While other OS such as FreeBSD have different name prefixes
and access control rules.

When remapping attributes on the host, it is important to
ensure that the remapping does not allow a guest user to
evade the guest access control rules.

Consider if `trusted.*` from the guest was remapped to
`user.virtiofs.trusted.*` in the host. An unprivileged
user in a Linux guest has the ability to write to xattrs
under `user.*`. Thus the user can evade the access
control restriction on `trusted.*` by instead writing
to `user.virtiofs.trusted.*`.

As noted above, the partitions used and access controls
applied, will vary across guest OS, so it is not wise to
try to predict what the guest OS will use.

The simplest way to avoid an insecure configuration is
to remap all xattrs at once, to a given fixed prefix.
This is shown in example (1) below.

If selectively mapping only a subset of xattr prefixes,
then rules must be added to explicitly block direct
access to the target of the remapping. This is shown
in example (2) below.

## Mapping examples

1. Prefix all attributes with `user.virtiofs.`

```shell
--xattrmap=":prefix:all::user.virtiofs.::bad:all:::"
```

This uses two rules, using : as the field separator;
the first rule prefixes and strips `user.virtiofs.`,
the second rule hides any non-prefixed attributes that
the host set.

This is equivalent to the `map` rule:

```shell
--xattrmap=":map::user.virtiofs.:"
```

2. Prefix `trusted.` attributes, allow others through

```shell
--xattrmap="/prefix/all/trusted./user.virtiofs./
            /bad/server//trusted./
            /bad/client/user.virtiofs.//
            /ok/all///"
```
(each rule is on a single line just for the sake of clarity)

Here there are four rules, using `/` as the field
separator, and also demonstrating that new lines can
be included between rules.
The first rule is the prefixing of `trusted.` and
stripping of `user.virtiofs.`.
The second rule hides unprefixed `trusted.` attributes
on the host.
The third rule stops a guest from explicitly setting
the `user.virtiofs.` path directly to prevent access
control bypass on the target of the earlier prefix
remapping.
Finally, the fourth rule lets all remaining attributes
through.

This is equivalent to the `map` rule:

```shell
--xattrmap="/map/trusted./user.virtiofs./"
```

3. Hide `security.` attributes, and allow everything else

```shell
--xattrmap="/bad/all/security./security./
            /ok/all///"
```

The first rule combines what could be separate client and server
rules into a single `all` rule, matching `security.` in either
client arguments or lists returned from the host.  This prevents
the client from seeing and/or setting any `security.` attributes on the server.